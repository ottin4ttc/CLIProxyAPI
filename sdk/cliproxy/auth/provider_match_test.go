package auth

import (
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestAuthMatchesProviderSelector(t *testing.T) {
	oauthAuth := &Auth{
		Provider:   "claude",
		Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth},
	}
	apiKeyAuth := &Auth{
		Provider:   "claude",
		Attributes: map[string]string{AttributeAPIKey: "sk-upstream"},
	}
	compatAuth := &Auth{
		Provider:   "openai-compatible-baidu",
		Attributes: map[string]string{AttributeAPIKey: "sk-upstream"},
	}

	tests := []struct {
		selector string
		auth     *Auth
		want     bool
	}{
		{"oauth", oauthAuth, true},
		{"oauth", apiKeyAuth, false},
		{"apikey", apiKeyAuth, true},
		{"apikey", oauthAuth, false},
		{"claude", oauthAuth, true},
		{"claude", apiKeyAuth, true},
		{"claude:oauth", oauthAuth, true},
		{"claude:oauth", apiKeyAuth, false},
		{"claude:apikey", apiKeyAuth, true},
		{"claude:apikey", oauthAuth, false},
		{"openai-compatible-*", compatAuth, true},
		{"openai-compatible-*:apikey", compatAuth, true},
		{"codex", compatAuth, false},
		{"", oauthAuth, false},
	}
	for _, tc := range tests {
		if got := authMatchesProviderSelector(tc.selector, tc.auth); got != tc.want {
			t.Fatalf("authMatchesProviderSelector(%q, provider=%q kind=%q) = %t, want %t",
				tc.selector, tc.auth.Provider, tc.auth.AuthKind(), got, tc.want)
		}
	}
}

func TestMatchProviderPattern(t *testing.T) {
	tests := []struct {
		pattern string
		value   string
		want    bool
	}{
		{"claude", "claude", true},
		{"claude", "codex", false},
		{"claude-*", "claude-api-key", true},
		{"*-baidu", "openai-compatible-baidu", true},
		{"openai-*-baidu", "openai-compatible-baidu", true},
		{"*", "anything", true},
		{"", "anything", false},
	}
	for _, tc := range tests {
		if got := matchProviderPattern(tc.pattern, tc.value); got != tc.want {
			t.Fatalf("matchProviderPattern(%q, %q) = %t, want %t", tc.pattern, tc.value, got, tc.want)
		}
	}
}

func TestExcludeProvidersFromMetadata(t *testing.T) {
	meta := map[string]any{
		cliproxyexecutor.ExcludeProvidersMetadataKey: []any{"APIKEY", " claude:oauth ", ""},
	}
	got := excludeProvidersFromMetadata(meta)
	if len(got) != 2 || got[0] != "apikey" || got[1] != "claude:oauth" {
		t.Fatalf("excludeProvidersFromMetadata() = %#v", got)
	}
	if got := excludeProvidersFromMetadata(nil); len(got) != 0 {
		t.Fatalf("nil metadata returned %#v", got)
	}
}
