package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"bitcord/server/internal/auth"
)

const (
	accessTokenTTL  = 15 * time.Minute
	refreshTokenTTL = 30 * 24 * time.Hour
)

var usernameRe = regexp.MustCompile(`^[a-z0-9_.]{3,32}$`)

type userPublic struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	IsAdmin     bool   `json:"is_admin"`
}

func issueTokens(ctx context.Context, s *Server, userID string, admin bool) (map[string]interface{}, error) {
	now := time.Now()
	acc, err := auth.SignToken(s.Cfg.JWTSecret, auth.Claims{
		Sub:  userID,
		Kind: "user",
		Iat:  now.Unix(),
		Exp:  now.Add(accessTokenTTL).Unix(),
	})
	if err != nil {
		return nil, err
	}
	rt := auth.NewRefreshToken()
	hash := auth.HashRefreshToken(rt)
	exp := now.Add(refreshTokenTTL)
	_, err = s.DB.Exec(ctx,
		`INSERT INTO refresh_tokens(user_id, token_hash, expires_at) VALUES($1,$2,$3)`,
		userID, hash, exp)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"access_token":  acc,
		"refresh_token": rt,
		"expires_in":    int(accessTokenTTL.Seconds()),
	}, nil
}

// 2.1 register
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.Cfg.RegistrationOpen {
		writeErr(w, http.StatusForbidden, "registration_closed", "registration is closed on this server")
		return
	}
	var body struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", "invalid JSON body")
		return
	}
	body.Username = strings.ToLower(strings.TrimSpace(body.Username))
	body.DisplayName = strings.TrimSpace(body.DisplayName)
	if body.DisplayName == "" {
		body.DisplayName = body.Username
	}
	if !usernameRe.MatchString(body.Username) {
		writeErr(w, http.StatusBadRequest, "bad_username", "username must be 3-32 chars: a-z, 0-9, _ .")
		return
	}
	if len(body.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "weak_password", "password must be at least 8 characters")
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "hashing failed")
		return
	}
	ctx := r.Context()

	// 2.5 first registered user becomes admin
	var count int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	isAdmin := count == 0

	var id string
	err = s.DB.QueryRow(ctx,
		`INSERT INTO users(username, display_name, password_hash, is_admin) VALUES($1,$2,$3,$4) RETURNING id`,
		body.Username, body.DisplayName, hash, isAdmin).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			writeErr(w, http.StatusConflict, "username_taken", "this username is already registered")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}

	tokens, err := issueTokens(ctx, s, id, isAdmin)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "token issue failed")
		return
	}
	tokens["user"] = userPublic{ID: id, Username: body.Username, DisplayName: body.DisplayName, IsAdmin: isAdmin}
	writeJSON(w, http.StatusCreated, tokens)
}

// 2.2 login
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", "invalid JSON body")
		return
	}
	body.Username = strings.ToLower(strings.TrimSpace(body.Username))
	ctx := r.Context()

	var id, pwHash, disp string
	var admin bool
	err := s.DB.QueryRow(ctx,
		`SELECT id, password_hash, display_name, is_admin FROM users WHERE username=$1`, body.Username).
		Scan(&id, &pwHash, &disp, &admin)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusUnauthorized, "bad_credentials", "wrong username or password")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	ok, err := auth.VerifyPassword(body.Password, pwHash)
	if err != nil || !ok {
		writeErr(w, http.StatusUnauthorized, "bad_credentials", "wrong username or password")
		return
	}
	tokens, err := issueTokens(ctx, s, id, admin)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "token issue failed")
		return
	}
	tokens["user"] = userPublic{ID: id, Username: body.Username, DisplayName: disp, IsAdmin: admin}
	writeJSON(w, http.StatusOK, tokens)
}

// 2.3 refresh
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.serverTokenOK(r) {
		writeErr(w, http.StatusUnauthorized, "server_token_required", "join this server first")
		return
	}
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RefreshToken == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "refresh_token required")
		return
	}
	ctx := r.Context()
	hash := auth.HashRefreshToken(body.RefreshToken)

	var userID string
	var admin bool
	err := s.DB.QueryRow(ctx,
		`SELECT u.id, u.is_admin FROM refresh_tokens rt JOIN users u ON u.id = rt.user_id::bigint
		 WHERE rt.token_hash=$1 AND rt.expires_at > now() AND rt.revoked_at IS NULL`, hash).
		Scan(&userID, &admin)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid_refresh", "refresh token invalid or expired")
		return
	}
	// rotate: revoke old, issue new pair
	if _, err := s.DB.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE token_hash=$1`, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	tokens, err := issueTokens(ctx, s, userID, admin)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "token issue failed")
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

// 2.4 logout: all=true -> revoke every session of the user
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
		All          bool   `json:"all"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	ctx := r.Context()
	uid := userIDFrom(r)
	var err error
	if body.All {
		_, err = s.DB.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE user_id=$1::bigint AND revoked_at IS NULL`, uid)
	} else if body.RefreshToken != "" {
		_, err = s.DB.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE token_hash=$1 AND user_id=$2::bigint`,
			auth.HashRefreshToken(body.RefreshToken), uid)
	} else {
		writeErr(w, http.StatusBadRequest, "bad_request", "refresh_token or all=true required")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// 4.1 search users
func (s *Server) handleSearchUsers(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	ctx := r.Context()
	var rows pgx.Rows
	var err error
	self := userIDFrom(r)
	if q == "" {
		rows, err = s.DB.Query(ctx,
			`SELECT id, username, display_name, is_admin FROM users WHERE id <> $1::bigint ORDER BY username LIMIT 25`, self)
	} else {
		like := "%" + q + "%"
		rows, err = s.DB.Query(ctx,
			`SELECT id, username, display_name, is_admin FROM users
			 WHERE id <> $1::bigint AND (username LIKE $2 OR display_name ILIKE $2) ORDER BY username LIMIT 25`, self, like)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	defer rows.Close()
	out := []userPublic{}
	for rows.Next() {
		var u userPublic
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName, &u.IsAdmin); err != nil {
			continue
		}
		out = append(out, u)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"users": out})
}

func isUniqueViolation(err error) bool {
	type constraintErr interface{ SQLState() string }
	// pgx maps 23505 to a PgError exposing SQLState via Error() string prefix; simplest robust check:
	msg := fmt.Sprint(err)
	return strings.Contains(msg, "23505") || strings.Contains(msg, "duplicate key")
}
