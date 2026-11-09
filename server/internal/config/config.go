package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// Config holds all server configuration, loaded from env vars or a .env file.
type Config struct {
	Addr            string   // HTTP listen address, e.g. ":8080"
	PublicURL       string   // externally visible base URL
	DBURL           string   // postgres connection string
	ServerName      string   // display name of this server
	ServerPassword  string   // optional join password (empty = not required)
	RegistrationOpen bool    // whether new users can register
	AllowedOrigins  []string // CORS allowed origins
	TLSCert         string   // path to TLS cert (empty = plain HTTP)
	TLSKey          string   // path to TLS key
	TurnSecret      string   // shared secret for coturn REST API
	TurnServer      string   // TURN server URL, e.g. turn:turn.example.com:3478
	TurnRealm       string   // TURN realm
	JWTSecret       string   // secret for signing access tokens
}

func Load() *Config {
	loadDotEnv(".env")

	c := &Config{
		Addr:             getEnv("BITCORD_ADDR", ":8080"),
		PublicURL:        getEnv("BITCORD_PUBLIC_URL", "http://localhost:8080"),
		DBURL:            getEnv("BITCORD_DB_URL", "postgres://postgres:postgres@localhost:5432/bitcord?sslmode=disable"),
		ServerName:       getEnv("BITCORD_SERVER_NAME", "My BitCord Server"),
		ServerPassword:   getEnv("BITCORD_SERVER_PASSWORD", ""),
		RegistrationOpen: getEnvBool("BITCORD_REGISTRATION_OPEN", true),
		AllowedOrigins:   splitCSV(getEnv("BITCORD_ALLOWED_ORIGINS", "")),
		TLSCert:          getEnv("BITCORD_TLS_CERT", ""),
		TLSKey:           getEnv("BITCORD_TLS_KEY", ""),
		TurnSecret:       getEnv("BITCORD_TURN_SECRET", ""),
		TurnServer:       getEnv("BITCORD_TURN_SERVER", ""),
		TurnRealm:        getEnv("BITCORD_TURN_REALM", ""),
		JWTSecret:        getEnv("BITCORD_JWT_SECRET", "change-me-dev-secret"),
	}
	return c
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getEnvBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadDotEnv reads simple KEY=VALUE lines from a file if it exists,
// without overriding already-set environment variables.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.Trim(strings.TrimSpace(line[eq+1:]), `"'`)
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}
