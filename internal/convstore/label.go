package convstore

import (
	"net/http"
	"strings"
)

// APIKeyFromHeaders extracts the client API key from the request headers the
// interceptor received. Checked in order: Authorization bearer token,
// X-Api-Key, X-Goog-Api-Key.
func APIKeyFromHeaders(h http.Header) string {
	if auth := strings.TrimSpace(h.Get("Authorization")); auth != "" {
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			return strings.TrimSpace(auth[len("bearer "):])
		}
		return auth
	}
	if key := strings.TrimSpace(h.Get("X-Api-Key")); key != "" {
		return key
	}
	return strings.TrimSpace(h.Get("X-Goog-Api-Key"))
}

// KeyLabel converts an API key into a human-readable directory name.
// Keys of 24 chars or fewer are used whole so user-added identity prefixes
// (e.g. "zhangsan-<key>") stay visible; longer keys keep the first 16 and
// last 4 characters.
func KeyLabel(apiKey string) string {
	key := strings.TrimSpace(apiKey)
	if key == "" {
		return "unknown"
	}
	if len(key) > 24 {
		key = key[:16] + "_" + key[len(key)-4:]
	}
	return sanitizeName(key)
}

// sanitizeName keeps [A-Za-z0-9._-] and replaces everything else with '-'
// so values are safe as file and directory names.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		return "unknown"
	}
	return out
}
