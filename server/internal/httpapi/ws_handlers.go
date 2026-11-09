package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"nhooyr.io/websocket"

	"bitcord/server/internal/auth"
	"bitcord/server/internal/ws"
)

// 3.1 GET /ws — upgrade; authorization via FIRST message (not URL).
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.serverTokenOK(r) {
		writeErr(w, http.StatusUnauthorized, "server_token_required", "join this server first")
		return
	}
	wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: len(s.Cfg.AllowedOrigins) == 0,
		OriginPatterns:     s.wsOriginPatterns(),
	})
	if err != nil {
		return // accept failed, nothing more to do
	}
	ctx := r.Context()

	// first message must be auth within 10 seconds
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, data, err := wsc.Read(wctx)
	cancel()
	if err != nil {
		wsc.Close(websocket.StatusPolicyViolation, "auth timeout")
		return
	}
	var hello struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &hello); err != nil || hello.Type != "auth" {
		wsc.Close(websocket.StatusPolicyViolation, "first message must be auth")
		return
	}
	claims, err := auth.ParseToken(s.Cfg.JWTSecret, hello.Token)
	if err != nil || claims.Kind != "user" {
		wsc.Close(websocket.StatusPolicyViolation, "bad token")
		return
	}
	userID := claims.Sub

	connID := make([]byte, 8)
	rand.Read(connID)
	c := ws.NewConn(userID, hex.EncodeToString(connID))
	c.Attach(wsc)
	s.Hub.Register(c)
	defer s.Hub.Unregister(c)

	// tell the client it's connected + last seen message id for catch-up (4.7)
	lastID := s.lastMessageID(ctx, userID)
	writeEnv(c, ws.Envelope{Type: "hello", Data: mustJSON(map[string]interface{}{
		"user_id":              userID,
		"conn_id":              c.ID,
		"last_message_id":      lastID,
		"server_time":          time.Now().UTC().Format(time.RFC3339),
	})})

	writerDone := make(chan struct{})
	go func() {
		c.WriteLoop(func() { close(writerDone) })
	}()

	// read loop with pong deadline
	for {
		rctx, rcancel := context.WithTimeout(ctx, 90*time.Second) // no traffic for 90s => dead (3.3)
		msgType, payload, err := wsc.Read(rctx)
		rcancel()
		if err != nil {
			break
		}
		if msgType != websocket.MessageText {
			continue
		}
		s.handleWSEvent(ctx, c, payload)
		select {
		case <-writerDone:
			return
		default:
		}
	}
}

func (s *Server) wsOriginPatterns() []string {
	if len(s.Cfg.AllowedOrigins) == 0 {
		return nil
	}
	return s.Cfg.AllowedOrigins
}

func writeEnv(c *ws.Conn, env ws.Envelope) {
	select {
	case c.send <- env:
	default:
	}
}

func mustJSON(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// dispatch inbound WS events
func (s *Server) handleWSEvent(ctx context.Context, c *ws.Conn, payload []byte) {
	var env ws.Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		writeEnv(c, ws.Envelope{Type: "error", Data: mustJSON(map[string]string{"message": "invalid json"})})
		return
	}
	switch env.Type {
	case "ping": // 3.5 test echo
		writeEnv(c, ws.Envelope{Type: "pong", ID: env.ID})
	case "message.send": // 4.3/4.4 send chat message over WS
		s.wsSendMessage(ctx, c, env)
	case "call.invite", "call.accept", "call.reject", "call.cancel", "call.hangup",
		"rtc.offer", "rtc.answer", "rtc.ice": // block 7
		s.wsCallEvent(ctx, c, env)
	case "read.mark": // 4.6 mark conversation read over WS
		s.wsMarkRead(ctx, c, env)
	default:
		writeEnv(c, ws.Envelope{Type: "error", ID: env.ID, Data: mustJSON(map[string]string{"message": "unknown type"})})
	}
}
