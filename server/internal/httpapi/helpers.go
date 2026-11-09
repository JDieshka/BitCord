package httpapi

import (
	"net"
	"strings"

	"bitcord/server/internal/auth"
)

func parseAccessToken(secret, token string) (*auth.Claims, error) {
	return auth.ParseToken(secret, token)
}

func remoteAddr_IP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

func strings_Split(s, sep string) []string { return strings.Split(s, sep) }
