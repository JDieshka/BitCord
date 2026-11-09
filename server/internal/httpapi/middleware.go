package httpapi

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

type ctxKey string

const userIDKey ctxKey = "user_id"
const usernameKey ctxKey = "username"
const isAdminKey ctxKey = "is_admin"

// requireUser validates Authorization: Bearer <access token> and the server join token.
func (s *Server) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.serverTokenOK(r) {
			writeErr(w, http.StatusUnauthorized, "server_token_required", "join this server first (X-Server-Token)")
			return
		}
		tok := bearer(r)
		claims, err := parseAccessToken(s.Cfg.JWTSecret, tok)
		if err != nil || claims.Kind != "user" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "valid access token required")
			return
		}
		ctx := context.WithValue(r.Context(), userIDKey, claims.Sub)
		next(w, r.WithContext(ctx))
	}
}

// optionalUser attaches user if a valid token is present; otherwise continues anonymously.
func (s *Server) optionalUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if t := bearer(r); t != "" {
			if claims, err := parseAccessToken(s.Cfg.JWTSecret, t); err == nil && claims.Kind == "user" {
				r = r.WithContext(context.WithValue(r.Context(), userIDKey, claims.Sub))
			}
		}
		next(w, r)
	}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && h[:7] == "Bearer " {
		return h[7:]
	}
	return ""
}

func userIDFrom(r *http.Request) string {
	v, _ := r.Context().Value(userIDKey).(string)
	return v
}

// ---- simple in-memory rate limiter keyed by IP ----

type limiter struct {
	rl     chi.Router // unused, kept simple
	mu     sync.Mutex
	events map[string][]time.Time
	limit  int
	window time.Duration
}

type rateLimiter struct {
	mu     sync.Mutex
	events map[string][]time.Time
	limit  int
	window time.Duration
}

func (s *Server) rateLimit(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{events: map[string][]time.Time{}, limit: limit, window: window}
}

func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		return strings_Split(xf, ",")[0]
	}
	return remoteAddr_IP(r.RemoteAddr)
}

func (l *rateLimiter) middlewareFunc(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := clientIP(r)
		l.mu.Lock()
		now := time.Now()
		cut := now.Add(-l.window)
		evs := l.events[key]
		kept := evs[:0]
		for _, t := range evs {
			if t.After(cut) {
				kept = append(kept, t)
			}
		}
		if len(kept) >= l.limit {
			l.mu.Unlock()
			writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many attempts, try again later")
			return
		}
		kept = append(kept, now)
		l.events[key] = kept
		l.mu.Unlock()
		next(w, r)
	}
}
