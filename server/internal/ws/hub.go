package ws

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// Envelope is the JSON frame used for every WS message. (3.5)
type Envelope struct {
	Type string          `json:"type"`
	ID   string          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Conn wraps a websocket connection with a send queue.
type Conn struct {
	UserID string
	ID     string // unique per-connection id
	ws     *websocket.Conn
	Send   chan Envelope
	once   sync.Once
	closed chan struct{}
}

// Attach sets the underlying websocket connection (called by the HTTP upgrade handler).
func (c *Conn) Attach(wsc *websocket.Conn) {
	c.ws = wsc
	c.closed = make(chan struct{})
	c.once = sync.Once{}
	if c.Send == nil {
		c.Send = make(chan Envelope, 64)
	}
}

// Hub keeps connections in memory: userID -> set of conns. Supports multiple tabs. (3.2)
type Hub struct {
	mu    sync.RWMutex
	byUser map[string]map[*Conn]bool
}

func NewHub() *Hub { return &Hub{byUser: map[string]map[*Conn]bool{}} }

// NewConn creates a connection handle; Attach must be called with the upgraded socket.
func NewConn(userID, id string) *Conn {
	return &Conn{
		UserID: userID,
		ID:     id,
		Send:   make(chan Envelope, 64),
		closed: make(chan struct{}),
	}
}

func (h *Hub) Register(c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byUser[c.UserID] == nil {
		h.byUser[c.UserID] = map[*Conn]bool{}
	}
	h.byUser[c.UserID][c] = true
}

func (h *Hub) Unregister(c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m, ok := h.byUser[c.UserID]; ok {
		delete(m, c)
		if len(m) == 0 {
			delete(h.byUser, c.UserID)
		}
	}
	c.Close()
}

func (h *Hub) Online(userIDs ...string) map[string]bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := map[string]bool{}
	for _, id := range userIDs {
		out[id] = len(h.byUser[id]) > 0
	}
	return out
}

// SendToUser delivers an event to all connections of a user (all tabs).
func (h *Hub) SendToUser(userID string, env Envelope) {
	h.mu.RLock()
	conns := make([]*Conn, 0, len(h.byUser[userID]))
	for c := range h.byUser[userID] {
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	for _, c := range conns {
		select {
		case c.Send <- env:
		default:
			// slow client: drop, writer will close on next timeout
		}
	}
}

// SendToUserExceptConn delivers to all connections of a user except one (multi-tab control).
func (h *Hub) SendToUserExceptConn(userID string, except *Conn, env Envelope) {
	h.mu.RLock()
	conns := make([]*Conn, 0, len(h.byUser[userID]))
	for c := range h.byUser[userID] {
		if c != except {
			conns = append(conns, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range conns {
		select {
		case c.Send <- env:
		default:
		}
	}
}

// SendToUsers broadcasts to many users.
func (h *Hub) SendToUsers(userIDs []string, env Envelope) {
	for _, u := range userIDs {
		h.SendToUser(u, env)
	}
}

func (c *Conn) Close() {
	c.once.Do(func() {
		close(c.closed)
		c.ws.Close(websocket.StatusNormalClosure, "")
	})
}

// WriteLoop pumps the send queue onto the socket; ping/pong keepalive. (3.3)
func (c *Conn) WriteLoop(done func()) {
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	ctx := c.ws.Context()
	for {
		select {
		case <-c.closed:
			return
		case env := <-c.Send:
			data, _ := json.Marshal(env)
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.ws.Write(wctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				c.Close()
				done()
				return
			}
		case <-ping.C:
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := c.ws.Ping(wctx)
			cancel()
			if err != nil {
				c.Close()
				done()
				return
			}
		}
	}
}
