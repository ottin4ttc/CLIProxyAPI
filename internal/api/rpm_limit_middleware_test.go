package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/apikeylimit"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func newRPMTestServer(limits config.APIKeyLimits) *Server {
	cfg := &config.Config{}
	cfg.APIKeyLimits = limits
	return &Server{cfg: cfg, rpmLimiter: apikeylimit.New()}
}

// runRPMRequest drives one request through the middleware with an already
// authenticated key, the way AuthMiddleware would leave it.
func runRPMRequest(t *testing.T, s *Server, method, path, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, path, nil)
	c.Set("userApiKey", apiKey)
	s.rpmLimitMiddleware()(c)
	return recorder
}

func TestRPMMiddlewareRejectsOverLimit(t *testing.T) {
	s := newRPMTestServer(config.APIKeyLimits{DefaultRPM: 2})
	for i := 0; i < 2; i++ {
		if got := runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-a").Code; got != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, got)
		}
	}
	recorder := runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-a")
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd request = %d, want 429", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry a Retry-After header")
	}
	if body := recorder.Body.String(); !strings.Contains(body, "rpm_limit_exceeded") {
		t.Fatalf("body = %q, want it to contain rpm_limit_exceeded", body)
	}
}

func TestRPMMiddlewareUnlimitedKeyPassesThrough(t *testing.T) {
	s := newRPMTestServer(config.APIKeyLimits{DefaultRPM: 0})
	for i := 0; i < 50; i++ {
		if got := runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-a").Code; got != http.StatusOK {
			t.Fatalf("request %d = %d, want 200 under an unlimited default", i, got)
		}
	}
}

func TestRPMMiddlewareExemptPathsAreNotCounted(t *testing.T) {
	s := newRPMTestServer(config.APIKeyLimits{DefaultRPM: 1})
	exempt := []struct{ method, path string }{
		{http.MethodGet, "/v1/models"},
		{http.MethodGet, "/v1beta/models"},
		{http.MethodGet, "/v1beta/models/gemini-3-pro"},
		{http.MethodPost, "/v1/messages/count_tokens"},
		{http.MethodGet, "/v1/live/call-123"},
		{http.MethodGet, "/v1/realtime"},
		{http.MethodGet, "/v1/realtime/calls/call-123"},
	}
	for _, tc := range exempt {
		for i := 0; i < 5; i++ {
			if got := runRPMRequest(t, s, tc.method, tc.path, "sk-a").Code; got != http.StatusOK {
				t.Fatalf("%s %s request %d = %d, want 200 (exempt path)", tc.method, tc.path, i, got)
			}
		}
	}
	// The counted budget of 1 must still be intact.
	if got := runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-a").Code; got != http.StatusOK {
		t.Fatalf("counted request = %d, want 200; exempt paths consumed the budget", got)
	}
	if got := runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-a").Code; got != http.StatusTooManyRequests {
		t.Fatalf("second counted request = %d, want 429", got)
	}
}

func TestRPMMiddlewarePostToModelsPathIsCounted(t *testing.T) {
	// Gemini generation is POST /v1beta/models/*; only GETs there are metadata.
	s := newRPMTestServer(config.APIKeyLimits{DefaultRPM: 1})
	if got := runRPMRequest(t, s, http.MethodPost, "/v1beta/models/gemini-3-pro:generateContent", "sk-a").Code; got != http.StatusOK {
		t.Fatalf("first generation = %d, want 200", got)
	}
	if got := runRPMRequest(t, s, http.MethodPost, "/v1beta/models/gemini-3-pro:generateContent", "sk-a").Code; got != http.StatusTooManyRequests {
		t.Fatalf("second generation = %d, want 429", got)
	}
}

func TestRPMMiddlewareIsolatesKeys(t *testing.T) {
	s := newRPMTestServer(config.APIKeyLimits{DefaultRPM: 1})
	runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-a")
	if got := runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-b").Code; got != http.StatusOK {
		t.Fatalf("sk-b = %d, want 200; one key's budget must not affect another", got)
	}
}

func TestRPMMiddlewareWithoutAuthenticatedKeyPassesThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := newRPMTestServer(config.APIKeyLimits{DefaultRPM: 1})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	s.rpmLimitMiddleware()(c)
	if c.IsAborted() {
		t.Fatal("a request with no userApiKey must pass through untouched")
	}
}

func TestThrottledRequestPublishesUsageRecord(t *testing.T) {
	// This is the whole point of the observability requirement: a throttled
	// request never reaches an executor, so unless the middleware publishes
	// this record the request leaves no trace downstream at all.
	captured := make(chan usage.Record, 4)
	usage.RegisterNamedPlugin("rpm-limit-test", usagePluginFunc(func(_ context.Context, r usage.Record) {
		captured <- r
	}))
	defer usage.RegisterNamedPlugin("rpm-limit-test", usagePluginFunc(func(context.Context, usage.Record) {}))

	s := newRPMTestServer(config.APIKeyLimits{DefaultRPM: 1})
	runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-a")
	if got := runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-a").Code; got != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", got)
	}

	// Delivery is asynchronous through the default usage manager.
	var record usage.Record
	select {
	case record = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("no usage record published for the throttled request")
	}
	if !record.Failed {
		t.Fatal("record.Failed = false, want true")
	}
	if record.Fail.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("record.Fail.StatusCode = %d, want 429", record.Fail.StatusCode)
	}
	if record.APIKey != "sk-a" {
		t.Fatalf("record.APIKey = %q, want sk-a; without it the sink cannot attribute the throttle", record.APIKey)
	}
	if !strings.Contains(record.Fail.Body, "rpm_limit_exceeded") {
		t.Fatalf("record.Fail.Body = %q, want it to mark the throttle", record.Fail.Body)
	}
}

// usagePluginFunc adapts a function to the usage.Plugin interface.
type usagePluginFunc func(context.Context, usage.Record)

func (f usagePluginFunc) HandleUsage(ctx context.Context, record usage.Record) { f(ctx, record) }

func TestAPIKeyHashPrefix(t *testing.T) {
	// The prefix is what gets logged and what joins to the usage sink's
	// api_key_hash; the key itself must never appear.
	got := apiKeyHashPrefix("sk-a")
	if len(got) != 12 {
		t.Fatalf("len = %d, want 12", len(got))
	}
	if strings.Contains(got, "sk-a") {
		t.Fatal("the hash must not embed the key")
	}
	if got == apiKeyHashPrefix("sk-b") {
		t.Fatal("different keys must hash differently")
	}
}
