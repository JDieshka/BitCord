package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// ---------- argon2id password hashing ----------

type HashParams struct {
	Time    uint32
	Memory  uint32
	Threads uint8
	KeyLen  uint32
}

var DefaultParams = HashParams{Time: 1, Memory: 64 * 1024, Threads: 4, KeyLen: 32}

func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	p := DefaultParams
	h := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	b64salt := base64.RawStdEncoding.EncodeToString(salt)
	b64hash := base64.RawStdEncoding.EncodeToString(h)
	return fmt.Sprintf("$argon2id$v=%d$t=%d,m=%d,p=%d$%s$%s",
		argon2.Version, p.Time, p.Memory, p.Threads, b64salt, b64hash), nil
}

var ErrMalformedHash = errors.New("malformed password hash")

func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrMalformedHash
	}
	var p HashParams
	if _, err := fmt.Sscanf(parts[3], "t=%d,m=%d,p=%d", &p.Time, &p.Memory, &p.Threads); err != nil {
		return false, ErrMalformedHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrMalformedHash
	}
	decodedHash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrMalformedHash
	}
	computedHash := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(decodedHash)))
	return subtle.ConstantTimeCompare(computedHash, decodedHash) == 1, nil
}

// ---------- access tokens (JWT-like HS256, compact implementation) ----------

type Claims struct {
	Sub  string `json:"sub"`  // subject: user id or "server:<sha>" token kind
	Kind string `json:"kind"` // "user" or "server"
	Exp  int64  `json:"exp"`
	Iat  int64  `json:"iat"`
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func SignToken(secret string, c Claims) (string, error) {
	header := b64([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	body := header + "." + b64(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	sig := b64(mac.Sum(nil))
	return body + "." + sig, nil
}

var ErrBadToken = errors.New("invalid token")

func ParseToken(secret, token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrBadToken
	}
	signing := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signing))
	expected := b64(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return nil, ErrBadToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrBadToken
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, ErrBadToken
	}
	if time.Now().Unix() > c.Exp {
		return nil, ErrBadToken
	}
	return &c, nil
}

// ---------- refresh tokens ----------

func NewRefreshToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func HashRefreshToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}
