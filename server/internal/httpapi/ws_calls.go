package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"bitcord/server/internal/calls"
	"bitcord/server/internal/ws"
)

func newCallID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "c_" + hex.EncodeToString(b)
}

// wsCallEvent handles call.* and rtc.* events on the socket. (7.1, 7.3)
func (s *Server) wsCallEvent(ctx context.Context, c *ws.Conn, env ws.Envelope) {
	var data struct {
		CallID      string          `json:"call_id"`
		PeerID      string          `json:"peer_id"` // target user for invite
		SDP         json.RawMessage `json:"sdp"`
		Candidate   json.RawMessage `json:"candidate"`
		Display     bool            `json:"display"` // screen-share flag passthrough
	}
	json.Unmarshal(env.Data, &data)

	switch env.Type {
	case "call.invite": // 7.1
		if data.PeerID == "" || data.PeerID == c.UserID {
			s.sendErr(c, env.ID, "bad_request", "peer_id required")
			return
		}
		st, busy := s.Calls.Invite(newCallID(), c.UserID, data.PeerID)
		if busy != "" { // 7.2 busy
			if busy == c.UserID {
				s.sendErr(c, env.ID, "busy", "you are already in a call")
				return
			}
			// callee busy: tell caller immediately
			writeEnv(c, ws.Envelope{Type: "call.busy", Data: mustJSON(map[string]string{"call_id": "", "peer_id": data.PeerID})})
			return
		}
		payload := map[string]interface{}{
			"call_id":    st.ID,
			"from":       c.UserID,
			"display":    data.Display,
		}
		// ring ALL tabs of callee (7.7)
		s.Hub.SendToUser(data.PeerID, ws.Envelope{Type: "call.incoming", Data: mustJSON(payload)})
		// confirm to caller all tabs
		s.Hub.SendToUser(c.UserID, ws.Envelope{Type: "call.ringing", Data: mustJSON(payload)})
		// 7.2 ringing timeout -> auto-cancel
		cid := st.ID
		st.RingTimer = time.AfterFunc(s.Calls.RingTimeout(), func() {
			cur := s.Calls.Get(cid)
			if cur != nil && cur.Status == "ringing" {
				from, to := s.Calls.End(cid)
				gone := mustJSON(map[string]interface{}{"call_id": cid, "reason": "no_answer"})
				if from != "" {
					s.Hub.SendToUser(from, ws.Envelope{Type: "call.ended", Data: gone})
				}
				if to != "" {
					s.Hub.SendToUser(to, ws.Envelope{Type: "call.ended", Data: gone})
				}
			}
		})

	case "call.accept": // 7.1 / 7.7 answer on one tab closes ringing on others
		st := s.Calls.Accept(data.CallID, c.UserID)
		if st == nil {
			s.sendErr(c, env.ID, "not_found", "no such ringing call")
			return
		}
		accepted := mustJSON(map[string]interface{}{"call_id": data.CallID, "by": c.UserID})
		// notify caller
		s.Hub.SendToUser(st.From, ws.Envelope{Type: "call.accepted", Data: accepted})
		// tell callee's OTHER tabs that it was answered elsewhere
		s.Hub.SendToUserExceptConn(c.UserID, c, ws.Envelope{Type: "call.answered_elsewhere", Data: accepted})

	case "call.reject":
		from, to := s.endCall(data.CallID)
		rej := mustJSON(map[string]interface{}{"call_id": data.CallID, "reason": "rejected"})
		if from != "" {
			s.Hub.SendToUser(from, ws.Envelope{Type: "call.rejected", Data: rej})
			s.Hub.SendToUser(from, ws.Envelope{Type: "call.ended", Data: rej})
		}
		if to != "" {
			s.Hub.SendToUser(to, ws.Envelope{Type: "call.ended", Data: rej})
		}

	case "call.cancel": // caller hangs up before answer
		from, to := s.endCall(data.CallID)
		cel := mustJSON(map[string]interface{}{"call_id": data.CallID, "reason": "cancelled"})
		for _, u := range []string{from, to} {
			if u != "" {
				s.Hub.SendToUser(u, ws.Envelope{Type: "call.ended", Data: cel})
			}
		}

	case "call.hangup": // either side after active
		from, to := s.endCall(data.CallID)
		hu := mustJSON(map[string]interface{}{"call_id": data.CallID, "reason": "hangup"})
		for _, u := range []string{from, to} {
			if u != "" {
				s.Hub.SendToUser(u, ws.Envelope{Type: "call.ended", Data: hu})
			}
		}

	case "rtc.offer", "rtc.answer", "rtc.ice": // 7.3 relay signalling verbatim
		st := s.Calls.Get(data.CallID)
		if st == nil {
			s.sendErr(c, env.ID, "not_found", "no active call")
			return
		}
		target := st.To
		if c.UserID == st.To {
			target = st.From
		}
		out := map[string]interface{}{
			"call_id": data.CallID,
			"from":    c.UserID,
		}
		if len(data.SDP) > 0 {
			out["sdp"] = json.RawMessage(data.SDP)
		}
		if len(data.Candidate) > 0 {
			out["candidate"] = json.RawMessage(data.Candidate)
		}
		s.Hub.SendToUser(target, ws.Envelope{Type: env.Type, Data: mustJSON(out)})
	}
}

func (s *Server) endCall(callID string) (string, string) {
	st := s.Calls.Get(callID)
	if st != nil && st.RingTimer != nil {
		st.RingTimer.Stop()
	}
	return s.Calls.End(callID)
}

func (s *Server) sendErr(c *ws.Conn, id, code, msg string) {
	writeEnv(c, ws.Envelope{Type: "error", ID: id, Data: mustJSON(map[string]string{"code": code, "message": msg})})
}

// used by WS disconnect hook (7.8): start grace period for each participant
func (s *Server) handleWSDisconnect(userID string) {
	callID := s.Calls.CallOf(userID)
	if callID == "" {
		return
	}
	st := s.Calls.Get(callID)
	if st == nil {
		return
	}
	onExpired := func() {
		f, t := s.endCall(callID)
		ended := mustJSON(map[string]interface{}{"call_id": callID, "reason": "disconnect"})
		for _, u := range []string{f, t} {
			if u != "" {
				s.Hub.SendToUser(u, ws.Envelope{Type: "call.ended", Data: ended})
			}
		}
	}
	s.Calls.OnDisconnect(callID, userID, onExpired)
}

var _ = calls.NewManager
