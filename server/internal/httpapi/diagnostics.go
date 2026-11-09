package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// 9.4 Diagnostics: DB connectivity, public URL reachability, TURN config & credential issuance.
func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	type check struct {
		Name    string `json:"name"`
		OK      bool   `json:"ok"`
		Detail  string `json:"detail,omitempty"`
	}
	checks := []check{}

	// 1. database
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var one int
	err := s.DB.QueryRow(ctx, "SELECT 1").Scan(&one)
	if err != nil {
		checks = append(checks, check{Name: "database", OK: false, Detail: err.Error()})
	} else {
		checks = append(checks, check{Name: "database", OK: true, Detail: "connected"})
	}

	// 2. public url configured
	if s.Cfg.PublicURL == "" {
		checks = append(checks, check{Name: "public_url", OK: false, Detail: "BITCORD_PUBLIC_URL is not set"})
	} else {
		checks = append(checks, check{Name: "public_url", OK: true, Detail: s.Cfg.PublicURL})
	}

	// 3. turn configured
	turnOK := s.Cfg.TurnSecret != "" && s.Cfg.TurnServer != ""
	detail := "not configured (calls will work with host/srflx candidates only)"
	if turnOK {
		detail = s.Cfg.TurnServer
	}
	checks = append(checks, check{Name: "turn", OK: turnOK, Detail: detail})

	// 4. turn credential issuance (HMAC scheme works)
	if turnOK {
		expiry := time.Now().Add(time.Hour).Unix()
		user := fmt.Sprintf("%d:%s", expiry, "diagnostics")
		cred, err := s.turnCredential(user)
		if err != nil || cred == "" {
			checks = append(checks, check{Name: "turn_credentials", OK: false, Detail: "credential generation failed"})
		} else {
			checks = append(checks, check{Name: "turn_credentials", OK: true, Detail: "HMAC-SHA1 issuance works"})
		}
	} else {
		checks = append(checks, check{Name: "turn_credentials", OK: false, Detail: "skipped: turn not configured"})
	}

	allOK := true
	for _, c := range checks {
		if !c.OK {
			allOK = false
		}
	}
	status := http.StatusOK
	if !allOK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]interface{}{
		"version":     ServerVersion,
		"api_version": APIVersion,
		"checks":      checks,
		"healthy":     allOK,
	})
}
