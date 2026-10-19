package webtools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

func testConfig(baseURL string) *config.Config {
	return &config.Config{SDKConfig: config.SDKConfig{WebTools: config.WebTools{
		Enabled: true,
		BaseURL: baseURL,
		APIKey:  "literal-key",
		Count:   8,
	}}}
}

func TestSearchAppliesDefaultsOnlyWhenClientOmitsThem(t *testing.T) {
	var seen []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = readAll(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"t","url":"https://example.com"}]}`))
	}))
	defer server.Close()

	client := NewClient(testConfig(server.URL))
	status, body, err := client.Search(context.Background(), []byte(`{"query":"golang"}`))
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if got := gjson.GetBytes(seen, "count").Int(); got != 8 {
		t.Fatalf("count = %d, want configured default 8; body=%s", got, seen)
	}
	if got := gjson.GetBytes(seen, "need_content").Bool(); got {
		t.Fatalf("need_content must stay absent when need-content is false; body=%s", seen)
	}

	// Explicit client values must win over the configured defaults.
	explicit := []byte(`{"query":"golang","count":2,"need_content":true}`)
	if _, _, errExplicit := client.Search(context.Background(), explicit); errExplicit != nil {
		t.Fatalf("Search explicit error: %v", errExplicit)
	}
	if got := gjson.GetBytes(seen, "count").Int(); got != 2 {
		t.Fatalf("count = %d, want client value 2; body=%s", got, seen)
	}
	if got := gjson.GetBytes(seen, "need_content"); !got.Bool() {
		t.Fatalf("need_content should stay true; body=%s", seen)
	}
}

func TestSearchForwardsUpstreamStatusAndBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
	}))
	defer server.Close()

	status, body, err := NewClient(testConfig(server.URL)).Search(context.Background(), []byte(`{"query":"x"}`))
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 passthrough", status)
	}
	if got := gjson.GetBytes(body, "error.message").String(); got != "rate limited" {
		t.Fatalf("upstream body was not passed through: %s", body)
	}
}

func TestSearchRequiresQuery(t *testing.T) {
	client := NewClient(testConfig("http://127.0.0.1:1"))
	_, _, err := client.Search(context.Background(), []byte(`{"count":3}`))
	if err == nil {
		t.Fatal("expected a request error for a missing query")
	}
	if _, ok := err.(*RequestError); !ok {
		t.Fatalf("error type = %T, want *RequestError", err)
	}
}

func TestCredentialAPIKeyBorrowsOpenAICompatibilityKey(t *testing.T) {
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{Name: "other", APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "other-key"}}},
			{Name: "deepseek", APIKeyEntries: []config.OpenAICompatibilityAPIKey{
				{APIKey: "  "},
				{APIKey: " deepseek-key "},
			}},
		},
		SDKConfig: config.SDKConfig{WebTools: config.WebTools{Enabled: true, Credential: " DeepSeek "}},
	}
	if got := credentialAPIKey(cfg, cfg.WebTools.Credential); got != "deepseek-key" {
		t.Fatalf("credentialAPIKey = %q, want trimmed first usable key", got)
	}
	if got := credentialAPIKey(cfg, "missing"); got != "" {
		t.Fatalf("credentialAPIKey(missing) = %q, want empty", got)
	}

	// The derived auth provider key resolves to the same entry.
	for _, alias := range []string{"openai-compatible-deepseek", "openai-compatibility-deepseek"} {
		if got := credentialAPIKey(cfg, alias); got != "deepseek-key" {
			t.Fatalf("credentialAPIKey(%q) = %q, want deepseek-key", alias, got)
		}
	}
}

func TestResolveFallsBackToDefaultBaseURLAndReportsMissingKey(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{WebTools: config.WebTools{Enabled: true}}}
	if _, err := NewClient(cfg).resolve(); err == nil {
		t.Fatal("expected an error when no upstream key resolves")
	}

	cfg.WebTools.APIKey = "k"
	resolved, err := NewClient(cfg).resolve()
	if err != nil {
		t.Fatalf("resolve error: %v", err)
	}
	if resolved.baseURL != DefaultBaseURL {
		t.Fatalf("baseURL = %q, want %q", resolved.baseURL, DefaultBaseURL)
	}
	if resolved.count != DefaultCount {
		t.Fatalf("count = %d, want %d", resolved.count, DefaultCount)
	}
}

func TestFetchPostsURLAndRejectsEmpty(t *testing.T) {
	var path string
	var seen []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		seen, _ = readAll(r)
		_, _ = w.Write([]byte(`{"title":"t","content":"c"}`))
	}))
	defer server.Close()

	client := NewClient(testConfig(server.URL))
	status, _, err := client.Fetch(context.Background(), []byte(`{"url":"https://example.com/a"}`))
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if status != http.StatusOK || path != "/fetch" {
		t.Fatalf("status = %d, path = %q", status, path)
	}
	if got := gjson.GetBytes(seen, "url").String(); got != "https://example.com/a" {
		t.Fatalf("fetch body = %s", seen)
	}

	if _, _, errEmpty := client.Fetch(context.Background(), []byte(`{}`)); errEmpty == nil {
		t.Fatal("expected a request error for a missing url")
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer func() {
		_ = r.Body.Close()
	}()
	return io.ReadAll(r.Body)
}

func TestSearchRequestBodyIsJSON(t *testing.T) {
	var seen []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = readAll(r)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	if _, _, err := NewClient(testConfig(server.URL)).Search(context.Background(), []byte(`{"query":"q","ori_question":"oq"}`)); err != nil {
		t.Fatalf("Search error: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(seen, &decoded); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%s)", err, seen)
	}
	if decoded["query"] != "q" || decoded["ori_question"] != "oq" {
		t.Fatalf("upstream body lost client fields: %s", seen)
	}
}
