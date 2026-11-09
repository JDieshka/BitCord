package calls

import (
	"encoding/json"
	"sync"
	"time"
)

// State of a 1:1 call, kept in memory. (7.1)
type CallState struct {
	ID        string
	From      string // caller user id
	To        string // callee user id
	Status    string // ringing | active | ended
	StartedAt time.Time
	RingTimer *time.Timer `json:"-"`
}

// Manager tracks active calls and per-user busy state.
type Manager struct {
	mu    sync.Mutex
	calls map[string]*CallState       // callID -> state
	inCall map[string]string          // userID -> callID (busy marker)
	// grace period after WS drop during active call (7.8)
	disconnected map[string]map[string]*time.Timer // callID -> userID -> timer
	grace        time.Duration
	ringTimeout  time.Duration
}

func NewManager() *Manager {
	return &Manager{
		calls:        map[string]*CallState{},
		inCall:       map[string]string{},
		disconnected: map[string]map[string]*time.Timer{},
		grace:        15 * time.Second,
		ringTimeout:  40 * time.Second,
	}
}

func (m *Manager) RingTimeout() time.Duration { return m.ringTimeout }
func (m *Manager) Grace() time.Duration       { return m.grace }

// Invite creates a call if neither side is busy. Returns (state, busyUser).
func (m *Manager) Invite(id, from, to string) (*CallState, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.inCall[from]; c != "" {
		return nil, from
	}
	if c := m.inCall[to]; c != "" {
		return nil, to
	}
	st := &CallState{ID: id, From: from, To: to, Status: "ringing", StartedAt: time.Now()}
	m.calls[id] = st
	m.inCall[from] = id
	m.inCall[to] = id
	return st, ""
}

func (m *Manager) Accept(callID, by string) *CallState {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.calls[callID]
	if st == nil || st.Status != "ringing" {
		return st
	}
	if st.To != by {
		return nil
	}
	st.Status = "active"
	if st.RingTimer != nil {
		st.RingTimer.Stop()
	}
	return st
}

// End removes the call; returns participants so events can be sent to all tabs.
func (m *Manager) End(callID string) (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.calls[callID]
	if st == nil {
		return "", ""
	}
	delete(m.calls, callID)
	delete(m.inCall, st.From)
	delete(m.inCall, st.To)
	for _, timers := range m.disconnected {
		delete(timers, callID)
	}
	return st.From, st.To
}

func (m *Manager) Get(callID string) *CallState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[callID]
}

func (m *Manager) CallOf(user string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inCall[user]
}

// OnDisconnect starts a grace timer for a user who dropped WS mid-call. (7.8)
// onExpired is called if the user hasn't reconnected within the grace period.
func (m *Manager) OnDisconnect(callID, userID string, onExpired func()) *time.Timer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disconnected[callID] == nil {
		m.disconnected[callID] = map[string]*time.Timer{}
	}
	t := time.AfterFunc(m.grace, onExpired)
	m.disconnected[callID][userID] = t
	return t
}

// OnReconnect cancels the grace timer if present; returns true if recovered.
func (m *Manager) OnReconnect(callID, userID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.disconnected[callID][userID]; ok {
		delete(m.disconnected[callID], userID)
		t.Stop()
		return true
	}
	return false
}

// Relay payload: generic signalling data forwarded between the two peers.
type Signal struct {
	CallID          string          `json:"call_id"`
	From            string          `json:"from"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	SDP             json.RawMessage `json:"sdp,omitempty"`
	Candidate       json.RawMessage `json:"candidate,omitempty"`
}
