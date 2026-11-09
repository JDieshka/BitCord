package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"bitcord/server/internal/calls"
	"bitcord/server/internal/config"
	"bitcord/server/internal/ws"
)

const APIVersion = "v1"

type Server struct {
	Cfg    *config.Config
	DB     *pgxpool.Pool
	Router chi.Router
	Hub    *ws.Hub
	Calls  *calls.Manager

	mu       sync.Mutex
	sessions map[string]time.Time // server join tokens -> expiry
}

func New(cfg *config.Config, pool *pgxpool.Pool) *Server {
	s := &Server{
		Cfg:      cfg,
		DB:       pool,
		Hub:      ws.NewHub(),
		Calls:    calls.NewManager(),
		sessions: map[string]time.Time{},
	}
	go s.cleanupSessions()
	return s
}

func (s *Server) cleanupSessions() {
	for range time.Tick(time.Minute) {
		s.mu.Lock()
		now := time.Now()
		for k, exp := range s.sessions {
			if now.After(exp) {
				delete(s.sessions, k)
			}
		}
		s.mu.Unlock()
	}
}

// CORS middleware with configurable allowed origins.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) originAllowed(origin string) bool {
	// Same-origin requests don't carry Origin in curl; browsers always send it.
	// If no allowlist configured, allow all (dev default). Otherwise exact match.
	if len(s.Cfg.AllowedOrigins) == 0 {
		return true
	}
	for _, o := range s.Cfg.AllowedOrigins {
		if strings.EqualFold(strings.TrimRight(o, "/"), strings.TrimRight(origin, "/")) {
			return true
		}
	}
	return false
}

// BuildRouter constructs the full HTTP router.
func (s *Server) BuildRouter() chi.Router {
	r := chi.NewRouter()
	r.Use(s.corsMiddleware)

	r.Get("/healthz", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	api := r.Route("/api/v1")
	api.Get("/server-info", s.handleServerInfo)
	api.Post("/server/join", s.handleServerJoin)

	// auth endpoints
	api.Post("/auth/register", s.rateLimit(10, time.Minute).middlewareFunc(s.handleRegister))
	api.Post("/auth/login", s.rateLimit(10, time.Minute).middlewareFunc(s.handleLogin))
	api.Post("/auth/refresh", s.handleRefresh)
	api.Post("/auth/logout", s.requireUser(s.handleLogout))

	// users
	api.Get("/users", s.requireUser(s.handleSearchUsers))

	// conversations & messages
	api.Post("/conversations/direct", s.requireUser(s.handleDirectConversation))
	api.Post("/conversations/group", s.requireUser(s.handleCreateGroup))
	api.Get("/conversations", s.requireUser(s.handleListConversations))
	api.Post("/conversations/{id}/members", s.requireUser(s.handleAddMembers))
	api.Delete("/conversations/{id}/members/{uid}", s.requireUser(s.handleRemoveMember))
	api.Post("/conversations/{id}/leave", s.requireUser(s.handleLeave))
	api.Get("/conversations/{id}/messages", s.requireUser(s.handleListMessages))
	api.Post("/conversations/{id}/messages", s.requireUser(s.handleSendMessage))
	api.Post("/conversations/{id}/read", s.requireUser(s.handleMarkRead))

	// turn credentials
	api.Get("/turn-credentials", s.requireUser(s.handleTurnCredentials))

	// diagnostics
	api.Get("/diagnostics", s.handleDiagnostics)

	// websocket
	api.Get("/ws", s.handleWS)

	s.Router = r
	return r
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]interface{}{"error": map[string]string{"code": code, "message": msg}})
}

// ---- server token check for protected endpoints when password_required ----

func (s *Server) serverTokenOK(r *http.Request) bool {
	if s.Cfg.ServerPassword == "" {
		return true
	}
	tok := r.Header.Get("X-Server-Token")
	if tok == "" {
		return false
	}
	s.mu.Lock()
	exp, ok := s.sessions[tok]
	s.mu.Unlock()
	return ok && time.Now().Before(exp)
}
