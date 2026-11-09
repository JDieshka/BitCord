package httpapi

import (
	"context"
	"encoding/json"

	"bitcord/server/internal/ws"
)

// 4.3/4.4 WS: message.send -> store, ack sender, push message.new to other members
func (s *Server) wsSendMessage(ctx context.Context, c *ws.Conn, env ws.Envelope) {
	var data struct {
		ClientMsgID    string `json:"client_msg_id"`
		ConversationID string `json:"conversation_id"`
		Body           string `json:"body"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || data.ConversationID == "" {
		writeEnv(c, ws.Envelope{Type: "error", ID: env.ID, Data: mustJSON(map[string]string{"message": "bad message.send payload"})})
		return
	}
	if !s.isMember(ctx, data.ConversationID, c.UserID) {
		writeEnv(c, ws.Envelope{Type: "error", ID: env.ID, Data: mustJSON(map[string]string{"code": "not_member", "message": "access denied"})})
		return
	}
	m, dup, err := s.insertMessage(ctx, data.ConversationID, c.UserID, data.Body, data.ClientMsgID)
	if err != nil {
		writeEnv(c, ws.Envelope{Type: "error", ID: env.ID, Data: mustJSON(map[string]string{"message": "save failed"})})
		return
	}
	// ack to sender (all sender's tabs get it so status updates everywhere)
	ackData := map[string]interface{}{"status": "sent"}
	if dup {
		ackData["status"] = "duplicate"
	}
	if data.ClientMsgID != "" {
		ackData["client_msg_id"] = data.ClientMsgID
	}
	ackData["message"] = m
	s.Hub.SendToUser(c.UserID, ws.Envelope{Type: "message.ack", ID: env.ID, Data: mustJSON(ackData)})

	// push to other members (4.4)
	members, _ := s.convMembers(ctx, data.ConversationID)
	newEnv := ws.Envelope{Type: "message.new", Data: mustJSON(m)}
	for _, uid := range members {
		if uid != c.UserID {
			s.Hub.SendToUser(uid, newEnv)
		}
	}
}

// 4.6 WS: read.mark {conversation_id}
func (s *Server) wsMarkRead(ctx context.Context, c *ws.Conn, env ws.Envelope) {
	var data struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || data.ConversationID == "" {
		return
	}
	var lastID string
	s.DB.QueryRow(ctx, `SELECT COALESCE(MAX(id)::text,'0') FROM messages WHERE conversation_id=$1::bigint`, data.ConversationID).Scan(&lastID)
	s.DB.Exec(ctx,
		`UPDATE conversation_members SET last_read_message_id = GREATEST(last_read_message_id, $3::bigint)
		 WHERE conversation_id=$1::bigint AND user_id=$2::bigint`, data.ConversationID, c.UserID, lastID)
	// tell all tabs of this user that unread is reset
	s.Hub.SendToUser(c.UserID, ws.Envelope{Type: "read.updated", Data: mustJSON(map[string]string{"conversation_id": data.ConversationID})})
}
