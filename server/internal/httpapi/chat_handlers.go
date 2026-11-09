package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"bitcord/server/internal/ws"
)

type conversation struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Title      string   `json:"title,omitempty"`
	MemberIDs  []string `json:"member_ids"`
	LastMsgID  *string  `json:"last_message_id,omitempty"`
	CreatorID  string   `json:"creator_id,omitempty"`
}

type messageOut struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	SenderID       string `json:"sender_id"`
	Body           string `json:"body"`
	ClientMsgID    string `json:"client_msg_id,omitempty"`
	CreatedAt      string `json:"created_at"`
}

// members of a conversation as string ids
func (s *Server) convMembers(ctx context.Context, convID string) ([]string, error) {
	rows, err := s.DB.Query(ctx, `SELECT user_id::text FROM conversation_members WHERE conversation_id=$1::bigint`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		out = append(out, id)
	}
	return out, nil
}

func (s *Server) isMember(ctx context.Context, convID, userID string) bool {
	var ok bool
	err := s.DB.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM conversation_members WHERE conversation_id=$1::bigint AND user_id=$2::bigint)`,
		convID, userID).Scan(&ok)
	return err == nil && ok
}

// 4.2 create-or-get direct conversation, unique per unordered pair
func (s *Server) handleDirectConversation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UserID == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "user_id required")
		return
	}
	me := userIDFrom(r)
	ctx := r.Context()

	// verify target exists
	var exists bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1::bigint)`, body.UserID).Scan(&exists); err != nil || !exists {
		writeErr(w, http.StatusNotFound, "no_user", "target user not found")
		return
	}
	if me == body.UserID {
		writeErr(w, http.StatusBadRequest, "self", "cannot chat with yourself")
		return
	}
	pair := directPair(me, body.UserID)

	var convID string
	err := s.DB.QueryRow(ctx, `SELECT id::text FROM conversations WHERE direct_pair=$1`, pair).Scan(&convID)
	if errors.Is(err, pgx.ErrNoRows) {
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		defer tx.Rollback(ctx)
		err = tx.QueryRow(ctx,
			`INSERT INTO conversations(kind, direct_pair, creator_id) VALUES('direct',$1,$2::bigint) RETURNING id::text`,
			pair, me).Scan(&convID)
		if err != nil {
			// race: another request created it concurrently
			if isUniqueViolation(err) {
				if qerr := s.DB.QueryRow(ctx, `SELECT id::text FROM conversations WHERE direct_pair=$1`, pair).Scan(&convID); qerr != nil {
					writeErr(w, http.StatusInternalServerError, "internal", "db error")
					return
				}
			} else {
				writeErr(w, http.StatusInternalServerError, "internal", "db error")
				return
			}
		}
		for _, uid := range []string{me, body.UserID} {
			if _, err := tx.Exec(ctx,
				`INSERT INTO conversation_members(conversation_id, user_id) VALUES($1::bigint,$2::bigint) ON CONFLICT DO NOTHING`,
				convID, uid); err != nil {
				writeErr(w, http.StatusInternalServerError, "internal", "db error")
				return
			}
		}
		if err := tx.Commit(ctx); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}

	members, _ := s.convMembers(ctx, convID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"conversation": conversation{ID: convID, Kind: "direct", MemberIDs: members},
	})
}

func directPair(a, b string) string {
	if strings.Compare(a, b) > 0 {
		a, b = b, a
	}
	return a + ":" + b
}

// 5.1 create group
func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title    string   `json:"title"`
		MemberIDs []string `json:"member_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Title) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "title required")
		return
	}
	me := userIDFrom(r)
	ctx := r.Context()

	ids := map[string]bool{me: true}
	for _, id := range body.MemberIDs {
		if v, err := strconv.ParseInt(id, 10, 64); err == nil && v > 0 {
			ids[id] = true
		}
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	defer tx.Rollback(ctx)

	var convID string
	err = tx.QueryRow(ctx,
		`INSERT INTO conversations(kind, title, creator_id) VALUES('group',$1,$2::bigint) RETURNING id::text`,
		body.Title, me).Scan(&convID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	memberList := []string{}
	for id := range ids {
		if _, err := tx.Exec(ctx,
			`INSERT INTO conversation_members(conversation_id, user_id) VALUES($1::bigint,$2::bigint) ON CONFLICT DO NOTHING`,
			convID, id); err != nil {
			continue
		}
		memberList = append(memberList, id)
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}

	// notify all members in real time (5.4)
	s.broadcastConversation(convID)
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"conversation": conversation{ID: convID, Kind: "group", Title: body.Title, MemberIDs: memberList, CreatorID: me},
	})
}

// 5.2 add members (only members can add; groups only)
func (s *Server) handleAddMembers(w http.ResponseWriter, r *http.Request) {
	convID := chi.URLParam(r, "id")
	me := userIDFrom(r)
	ctx := r.Context()
	if !s.isMember(ctx, convID, me) {
		writeErr(w, http.StatusForbidden, "not_member", "you are not a member of this conversation") // 5.3 server-side check
		return
	}
	var kind string
	if err := s.DB.QueryRow(ctx, `SELECT kind FROM conversations WHERE id=$1::bigint`, convID).Scan(&kind); err != nil || kind != "group" {
		writeErr(w, http.StatusBadRequest, "not_group", "only groups support adding members")
		return
	}
	var body struct {
		MemberIDs []string `json:"member_ids"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	added := []string{}
	for _, id := range body.MemberIDs {
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			continue
		}
		res, err := s.DB.Exec(ctx,
			`INSERT INTO conversation_members(conversation_id, user_id) SELECT $1::bigint, u.id FROM users u WHERE u.id=$2::bigint
			 ON CONFLICT DO NOTHING`, convID, id)
		if err == nil && res.RowsAffected() > 0 {
			added = append(added, id)
		}
	}
	if len(added) > 0 {
		s.broadcastConversation(convID)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"added": added})
}

// 5.2 remove member (owner or any member can remove others? keep simple: owner/admin only)
func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	convID := chi.URLParam(r, "id")
	uid := chi.URLParam(r, "uid")
	me := userIDFrom(r)
	ctx := r.Context()
	if !s.isMember(ctx, convID, me) {
		writeErr(w, http.StatusForbidden, "not_member", "you are not a member")
		return
	}
	var creator string
	var kind string
	if err := s.DB.QueryRow(ctx, `SELECT COALESCE(creator_id::text,''), kind FROM conversations WHERE id=$1::bigint`, convID).Scan(&creator, &kind); err != nil || kind != "group" {
		writeErr(w, http.StatusBadRequest, "not_group", "not a group")
		return
	}
	if creator != me {
		writeErr(w, http.StatusForbidden, "not_owner", "only the group owner can remove members")
		return
	}
	if _, err := s.DB.Exec(ctx, `DELETE FROM conversation_members WHERE conversation_id=$1::bigint AND user_id=$2::bigint`, convID, uid); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	s.broadcastConversation(convID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// 5.2 leave group / conversation
func (s *Server) handleLeave(w http.ResponseWriter, r *http.Request) {
	convID := chi.URLParam(r, "id")
	me := userIDFrom(r)
	ctx := r.Context()
	if !s.isMember(ctx, convID, me) {
		writeErr(w, http.StatusNotFound, "not_found", "not a member")
		return
	}
	if _, err := s.DB.Exec(ctx, `DELETE FROM conversation_members WHERE conversation_id=$1::bigint AND user_id=$2::bigint`, convID, me); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	s.broadcastConversation(convID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// list my conversations with unread counters (4.6)
func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	me := userIDFrom(r)
	ctx := r.Context()
	rows, err := s.DB.Query(ctx, `
		SELECT c.id::text, c.kind, COALESCE(c.title,''), COALESCE(c.last_message_id::text,''), m.last_read_message_id::bigint
		FROM conversations c
		JOIN conversation_members m ON m.conversation_id = c.id AND m.user_id = $1::bigint`, me)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	defer rows.Close()
	type convOut struct {
		ID        string   `json:"id"`
		Kind      string   `json:"kind"`
		Title     string   `json:"title,omitempty"`
		MemberIDs []string `json:"member_ids"`
		Unread    int      `json:"unread"`
		Peers     []userPublic `json:"peers,omitempty"`
	}
	out := []convOut{}
	for rows.Next() {
		var id, kind, title, lastMsg string
		var lastRead int64
		if err := rows.Scan(&id, &kind, &title, &lastMsg, &lastRead); err != nil {
			continue
		}
		members, _ := s.convMembers(ctx, id)
		unread := 0
		if lastMsg != "" {
			var n int
			s.DB.QueryRow(ctx,
				`SELECT count(*) FROM messages WHERE conversation_id=$1::bigint AND id > $2 AND sender_id <> $3::bigint`,
				id, lastRead, me).Scan(&n)
			unread = n
		}
		c := convOut{ID: id, Kind: kind, Title: title, MemberIDs: members, Unread: unread}
		if kind == "direct" {
			for _, m := range members {
				if m != me {
					var u userPublic
					if err := s.DB.QueryRow(ctx, `SELECT id::text, username, display_name, is_admin FROM users WHERE id=$1::bigint`, m).
						Scan(&u.ID, &u.Username, &u.DisplayName, &u.IsAdmin); err == nil {
						c.Peers = append(c.Peers, u)
					}
				}
			}
		}
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"conversations": out})
}

// 4.5 cursor pagination: ?before_id=X&limit=20 or ?after_id=X for catch-up (4.7)
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	convID := chi.URLParam(r, "id")
	me := userIDFrom(r)
	ctx := r.Context()
	if !s.isMember(ctx, convID, me) {
		writeErr(w, http.StatusForbidden, "not_member", "access denied") // 5.3
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	before := r.URL.Query().Get("before_id")
	after := r.URL.Query().Get("after_id")

	var rows pgx.Rows
	var err error
	if after != "" {
		rows, err = s.DB.Query(ctx,
			`SELECT id::text, conversation_id::text, sender_id::text, body, COALESCE(client_msg_id,''), created_at
			 FROM messages WHERE conversation_id=$1::bigint AND id > $2::bigint ORDER BY id ASC LIMIT $3`,
			convID, after, limit)
	} else if before != "" {
		rows, err = s.DB.Query(ctx,
			`SELECT id, conv, sender, body, cmid, created_at FROM (
				SELECT id::text, conversation_id::text, sender_id::text, body, COALESCE(client_msg_id,'') AS cmid, created_at
				FROM messages WHERE conversation_id=$1::bigint AND id < $2::bigint ORDER BY id DESC LIMIT $3
			 ) t ORDER BY id DESC NULLS LAST`, convID, before, limit)
	} else {
		rows, err = s.DB.Query(ctx,
			`SELECT id, conv, sender, body, cmid, created_at FROM (
				SELECT id::text, conversation_id::text, sender_id::text, body, COALESCE(client_msg_id,'') AS cmid, created_at
				FROM messages WHERE conversation_id=$1::bigint ORDER BY id DESC LIMIT $2
			 ) t ORDER BY id DESC NULLS LAST`, convID, limit)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	defer rows.Close()
	msgs := []messageOut{}
	for rows.Next() {
		var m messageOut
		var ts interface{}
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.SenderID, &m.Body, &m.ClientMsgID, &ts); err != nil {
			continue
		}
		m.CreatedAt = fmt.Sprint(ts)
		msgs = append(msgs, m)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"messages": msgs})
}

// insertMessage stores a message with dedupe by (sender_id, client_msg_id). Returns existing row on dup. (4.3)
func (s *Server) insertMessage(ctx context.Context, convID, senderID, body, clientMsgID string) (*messageOut, bool, error) {
	if body == "" || len(body) > 4000 {
		return nil, false, errors.New("invalid body")
	}
	var m messageOut
	q := `INSERT INTO messages(conversation_id, sender_id, body, client_msg_id)
	      VALUES($1::bigint,$2::bigint,$3,NULLIF($4,''))
	      RETURNING id::text, conversation_id::text, sender_id::text, body, COALESCE(client_msg_id,''), created_at::text`
	err := s.DB.QueryRow(ctx, q, convID, senderID, body, clientMsgID).
		Scan(&m.ID, &m.ConversationID, &m.SenderID, &m.Body, &m.ClientMsgID, &m.CreatedAt)
	if err != nil && isUniqueViolation(err) && clientMsgID != "" {
		// duplicate delivery — return the already stored message
		derr := s.DB.QueryRow(ctx,
			`SELECT id::text, conversation_id::text, sender_id::text, body, COALESCE(client_msg_id,''), created_at::text
			 FROM messages WHERE sender_id=$1::bigint AND client_msg_id=$2`, senderID, clientMsgID).
			Scan(&m.ID, &m.ConversationID, &m.SenderID, &m.Body, &m.ClientMsgID, &m.CreatedAt)
		if derr != nil {
			return nil, false, derr
		}
		return &m, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	// update conversation.last_message_id
	s.DB.Exec(ctx, `UPDATE conversations SET last_message_id=$2::bigint WHERE id=$1::bigint`, convID, m.ID)
	return &m, false, nil
}

// HTTP send message (fallback path; WS path in ws_chat.go)
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	convID := chi.URLParam(r, "id")
	me := userIDFrom(r)
	ctx := r.Context()
	if !s.isMember(ctx, convID, me) {
		writeErr(w, http.StatusForbidden, "not_member", "access denied")
		return
	}
	var body struct {
		Body        string `json:"body"`
		ClientMsgID string `json:"client_msg_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", "invalid JSON")
		return
	}
	m, dup, err := s.insertMessage(ctx, convID, me, body.Body, body.ClientMsgID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not save message")
		return
	}
	members, _ := s.convMembers(ctx, convID)
	env := ws.Envelope{Type: "message.new", Data: mustJSON(m)}
	for _, uid := range members {
		if uid != me {
			s.Hub.SendToUser(uid, env)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"message": m, "duplicate": dup})
}

// 4.6 mark read
func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	convID := chi.URLParam(r, "id")
	me := userIDFrom(r)
	ctx := r.Context()
	var lastID string
	if err := s.DB.QueryRow(ctx, `SELECT COALESCE(MAX(id)::text,'0') FROM messages WHERE conversation_id=$1::bigint`, convID).Scan(&lastID); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	if _, err := s.DB.Exec(ctx,
		`UPDATE conversation_members SET last_read_message_id = GREATEST(last_read_message_id, $3::bigint)
		 WHERE conversation_id=$1::bigint AND user_id=$2::bigint`, convID, me, lastID); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// broadcast conversation.updated to all current members (5.4)
func (s *Server) broadcastConversation(convID string) {
	ctx := context.Background()
	members, err := s.convMembers(ctx, convID)
	if err != nil {
		return
	}
	var title, kind string
	s.DB.QueryRow(ctx, `SELECT kind, COALESCE(title,'') FROM conversations WHERE id=$1::bigint`, convID).Scan(&kind, &title)
	data := mustJSON(map[string]interface{}{
		"conversation_id": convID,
		"kind":            kind,
		"title":           title,
		"member_ids":      members,
	})
	env := ws.Envelope{Type: "conversation.updated", Data: data}
	for _, uid := range members {
		s.Hub.SendToUser(uid, env)
	}
}

// lastMessageID for hello payload (catch-up watermark) (4.7)
func (s *Server) lastMessageID(ctx context.Context, userID string) string {
	var max string
	s.DB.QueryRow(ctx,
		`SELECT COALESCE(MAX(m.id)::text,'0') FROM messages m
		 JOIN conversation_members cm ON cm.conversation_id = m.conversation_id AND cm.user_id = m.sender_id
		 WHERE cm.user_id=$1::bigint OR m.conversation_id IN (SELECT conversation_id FROM conversation_members WHERE user_id=$1::bigint)`,
		userID).Scan(&max)
	if max == "" {
		return "0"
	}
	return max
}
