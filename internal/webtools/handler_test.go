package webtools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func newTestRouter(cfg *config.Config) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewHandler(func() *config.Config { return cfg })
	router.POST("/v1/tools/search", handler.Search)
	router.POST("/v1/tools/fetch", handler.Fetch)
	return router
}

func TestGatewayDisabledAnswers404(t *testing.T) {
	router := newTestRouter(&config.Config{})
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/search", strings.NewReader(`{"query":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when the gateway is disabled", rec.Code)
	}
}

func TestGatewayRejectsInvalidJSON(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{WebTools: config.WebTools{Enabled: true, APIKey: "k"}}}
	router := newTestRouter(cfg)
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/search", strings.NewReader(`{"query":`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := decodeErrorMessage(t, rec.Body.Bytes()); got == "" {
		t.Fatal("expected an error message")
	}
}

func TestGatewayPassesUpstreamSuccessThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("Authorization = %q, want the configured key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Title","url":"https://example.com","summary":"Sum"}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{WebTools: config.WebTools{
		Enabled: true, BaseURL: upstream.URL, APIKey: "k",
	}}}
	router := newTestRouter(cfg)
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/search", strings.NewReader(`{"query":"x"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var decoded struct {
		Results []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(decoded.Results) != 1 || decoded.Results[0].URL != "https://example.com" {
		t.Fatalf("unexpected results: %s", rec.Body.String())
	}
}

func TestGatewayMapsMissingUpstreamKeyTo503(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{WebTools: config.WebTools{Enabled: true}}}
	router := newTestRouter(cfg)
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/search", strings.NewReader(`{"query":"x"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for a deployment fault", rec.Code)
	}
}

func TestGatewayLiveConfigReload(t *testing.T) {
	var seenPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{SDKConfig: config.SDKConfig{WebTools: config.WebTools{APIKey: "k"}}}
	router := newTestRouter(cfg)

	disabled := func() int {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/tools/search", strings.NewReader(`{"query":"x"}`)))
		return rec.Code
	}
	if got := disabled(); got != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 before enabling", got)
	}

	// The handler reads the live config, so a hot reload applies in place.
	cfg.WebTools.Enabled = true
	cfg.WebTools.BaseURL = upstream.URL
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/tools/search", strings.NewReader(`{"query":"x"}`)))
	if rec.Code != http.StatusOK || seenPath != "/search" {
		t.Fatalf("status = %d, path = %q", rec.Code, seenPath)
	}
}

func decodeErrorMessage(t *testing.T, body []byte) string {
	t.Helper()
	var decoded struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, body)
	}
	if decoded.Error.Type == "" {
		t.Fatalf("error type is empty: %s", body)
	}
	return decoded.Error.Message
}
