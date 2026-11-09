package httpapi

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// 6.2 TURN REST API temporary credentials: username = "<expiry-unix>:<user_id>",
// credential = base64(HMAC-SHA1(secret, username)). The static shared secret never leaves the server. (6.3)
func (s *Server) handleTurnCredentials(w http.ResponseWriter, r *http.Request) {
	if s.Cfg.TurnSecret == "" || s.Cfg.TurnServer == "" {
		writeErr(w, http.StatusNotFound, "turn_not_configured", "TURN is not configured on this server")
		return
	}
	userID := userIDFrom(r)
	ttl := 12 * time.Hour
	expiry := time.Now().Add(ttl).Unix()
	username := fmt.Sprintf("%d:%s", expiry, userID)
	credential, _ := s.turnCredential(username)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ice_servers": []map[string]string{{
			"urls":       s.Cfg.TurnServer,
			"username":   username,
			"credential": credential,
		}},
		"ttl": strconv.FormatInt(int64(ttl.Seconds()), 10),
	})
}

// turnCredential computes the coturn REST API HMAC-SHA1 credential for a username.
func (s *Server) turnCredential(username string) (string, error) {
	mac := hmac.New(sha1.New, []byte(s.Cfg.TurnSecret))
	_, err := mac.Write([]byte(username))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}
