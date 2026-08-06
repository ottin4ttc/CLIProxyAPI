package convstore

import (
	"net/http"
	"testing"
)

func TestAPIKeyFromHeaders(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
		want string
	}{
		{"bearer", http.Header{"Authorization": {"Bearer sk-abc123"}}, "sk-abc123"},
		{"x-api-key", http.Header{"X-Api-Key": {"sk-xyz"}}, "sk-xyz"},
		{"goog", http.Header{"X-Goog-Api-Key": {"AIza-123"}}, "AIza-123"},
		{"none", http.Header{}, ""},
	}
	for _, c := range cases {
		if got := APIKeyFromHeaders(c.h); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestKeyLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"short-key", "short-key"},                               // <=24: whole
		{"zhangsan-sk-abcdefgh1234", "zhangsan-sk-abcdefgh1234"}, // exactly 24: whole
		{"zhangsan-sk-abcdefgh12345", "zhangsan-sk-abcd_2345"},   // 25: 16+_+4
		{"a b/c\\d", "a-b-c-d"},                                  // sanitized
		{"", "unknown"},
	}
	for _, c := range cases {
		if got := KeyLabel(c.in); got != c.want {
			t.Errorf("KeyLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
