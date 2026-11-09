package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"
)

type serverInfo struct {
	Name             string `json:"name"`
	Version          string `json:"version"`
	APIVersion       string `json:"api_version"`
	PasswordRequired bool   `json:"password_required"`
	RegistrationOpen bool   `json:"registration_open"`
}

// 1.1 public server info
func (s *Server) handleServerInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, serverInfo{
		Name:             s.Cfg.ServerName,
		Version:          ServerVersion,
		APIVersion:       APIVersion,
		PasswordRequired: s.Cfg.ServerPassword != "",
		RegistrationOpen: s.Cfg.RegistrationOpen,
	})
}

// 1.4 join with server password -> short-lived server token (24h)
func (s *Server) handleServerJoin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	if s.Cfg.ServerPassword == "" {
		writeErr(w, http.StatusBadRequest, "no_password", "this server does not require a password")
		return
	}
	want := sha256.Sum256([]byte(s.Cfg.ServerPassword))
	got := sha256.Sum256([]byte(body.Password))
	if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		writeErr(w, http.StatusUnauthorized, "bad_password", "incorrect server password")
		return
	}
	exp := time.Now().Add(24 * time.Hour)
	tokBytes := make([]byte, 32)
	rand.Read(tokBytes)
	secret := base64.RawURLEncoding.EncodeToString(tokBytes)
	s.mu.Lock()
	s.sessions[secret] = exp
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"server_token": secret,
		"expires_at":   exp.UTC().Format(time.RFC3339),
	})
}
