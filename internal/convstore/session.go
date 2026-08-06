package convstore

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// ExtractSession determines the session grouping key for a request.
// Priority: explicit client session id (Claude header, Codex headers,
// Claude metadata.user_id) then a per-request fingerprint of api key + full
// body + UTC date bucket. Header-less requests are deliberately NOT
// aggregated: a stable prefix fingerprint (system + first user message) was
// tried first, but template-prompt batch apps collapse a whole day of
// unrelated requests into one ever-growing file. Identical bodies (retries)
// still share a file; each distinct request is otherwise self-contained
// because full-history clients resend the entire transcript every turn.
// sessionKey is always non-empty and filename-safe; sessionID is the raw
// extracted id ("" for fingerprints).
func ExtractSession(apiKey string, headers http.Header, body []byte, utcDate string) (sessionKey, sessionID, source string) {
	if id := strings.TrimSpace(headers.Get("X-Claude-Code-Session-Id")); id != "" {
		return sanitizeName(id), id, "claude-header"
	}
	if id := codexSessionHeader(headers); id != "" {
		return sanitizeName(id), id, "codex-header"
	}
	if uid := gjson.GetBytes(body, "metadata.user_id").String(); uid != "" {
		if idx := strings.LastIndex(uid, "session_"); idx >= 0 {
			if id := strings.TrimSpace(uid[idx+len("session_"):]); id != "" {
				return sanitizeName(id), id, "claude-metadata"
			}
		}
	}
	sum := sha256.Sum256(append([]byte(apiKey+"\x00"), body...))
	return "fp-" + hex.EncodeToString(sum[:8]) + "-" + utcDate, "", "fingerprint"
}

// codexSessionHeader scans headers case-insensitively for Codex-style
// session identifiers, tolerating '-' vs '_' spelling differences.
func codexSessionHeader(headers http.Header) string {
	for name, values := range headers {
		normalized := strings.ReplaceAll(strings.ToLower(name), "-", "_")
		if normalized != "session_id" && normalized != "conversation_id" {
			continue
		}
		for _, v := range values {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	return ""
}
