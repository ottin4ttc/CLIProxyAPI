package convstore

import (
	"net/http"
	"strings"
	"testing"
)

const day = "2026-07-13"

func TestExtractSessionClaudeHeader(t *testing.T) {
	h := http.Header{"X-Claude-Code-Session-Id": {"9d3e0a1b-uuid"}}
	key, id, source := ExtractSession("sk-a", h, []byte(`{}`), day)
	if key != "9d3e0a1b-uuid" || id != "9d3e0a1b-uuid" || source != "claude-header" {
		t.Fatalf("got %q %q %q", key, id, source)
	}
}

func TestExtractSessionCodexHeader(t *testing.T) {
	// Codex clients send session_id / conversation_id with varying spellings.
	for _, name := range []string{"Session_id", "session_id", "Session-Id", "Conversation_id"} {
		h := http.Header{}
		h[name] = []string{"conv-42"}
		key, id, source := ExtractSession("sk-a", h, []byte(`{}`), day)
		if key != "conv-42" || id != "conv-42" || source != "codex-header" {
			t.Fatalf("%s: got %q %q %q", name, key, id, source)
		}
	}
}

func TestExtractSessionClaudeMetadata(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"user_abc_account__session_f00d-uuid"}}`)
	key, id, source := ExtractSession("sk-a", http.Header{}, body, day)
	if key != "f00d-uuid" || id != "f00d-uuid" || source != "claude-metadata" {
		t.Fatalf("got %q %q %q", key, id, source)
	}
}

func TestExtractSessionFingerprintPerRequest(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hello"}]}`)
	key1, id, source := ExtractSession("sk-a", http.Header{}, body, day)
	if id != "" || source != "fingerprint" {
		t.Fatalf("got id=%q source=%q", id, source)
	}
	if !strings.HasPrefix(key1, "fp-") || !strings.HasSuffix(key1, "-"+day) {
		t.Fatalf("unexpected key %q", key1)
	}
	// Identical body (e.g. a retry) -> same key.
	keyDup, _, _ := ExtractSession("sk-a", http.Header{}, body, day)
	if key1 != keyDup {
		t.Fatalf("identical body must fingerprint identically: %q vs %q", key1, keyDup)
	}
	// Different body (longer history, another task, ...) -> different key.
	// Header-less clients get one file per distinct request; template-prompt
	// batch apps must not collapse into one ever-growing per-day file.
	body2 := []byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"more"}]}`)
	key2, _, _ := ExtractSession("sk-a", http.Header{}, body2, day)
	if key1 == key2 {
		t.Fatal("distinct bodies must not share a fingerprint")
	}
	// Different api key -> different fingerprint.
	key3, _, _ := ExtractSession("sk-b", http.Header{}, body, day)
	if key1 == key3 {
		t.Fatal("fingerprint must include api key")
	}
	// Different day -> different bucket.
	key4, _, _ := ExtractSession("sk-a", http.Header{}, body, "2026-07-14")
	if key1 == key4 {
		t.Fatal("fingerprint must include date bucket")
	}
}

func TestExtractSessionRawFallback(t *testing.T) {
	key, _, source := ExtractSession("sk-a", http.Header{}, []byte("not json"), day)
	if source != "fingerprint" || !strings.HasPrefix(key, "fp-") {
		t.Fatalf("got %q %q", key, source)
	}
}
