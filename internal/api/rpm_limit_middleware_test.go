package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/apikeylimit"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func newRPMTestServer(limits config.APIKeyLimits) *Server {
	cfg := &config.Config{}
	cfg.APIKeyLimits = limits
	return &Server{cfg: cfg, rpmLimiter: apikeylimit.New()}
}

// rpmRequestResult reports both what the middleware wrote to the response and
// whether it aborted the gin context. recorder.Code is 200 by default even
// when the middleware never calls c.Next(), so a pass-through assertion must
// check Aborted, not Code — Code alone can't distinguish "handler ran" from
// "handler was never reached".
type rpmRequestResult struct {
	aborted  bool
	recorder *httptest.ResponseRecorder
}

// runRPMRequest drives one request through the middleware with an already
// authenticated key, the way AuthMiddleware would leave it.
func runRPMRequest(t *testing.T, s *Server, method, path, apiKey string) rpmRequestResult {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, path, nil)
	c.Set("userApiKey", apiKey)
	s.rpmLimitMiddleware()(c)
	return rpmRequestResult{aborted: c.IsAborted(), recorder: recorder}
}

func TestRPMMiddlewareRejectsOverLimit(t *testing.T) {
	s := newRPMTestServer(config.APIKeyLimits{DefaultRPM: 2})
	for i := 0; i < 2; i++ {
		if got := runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-a"); got.aborted {
			t.Fatalf("request %d aborted, want pass-through", i)
		}
	}
	result := runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-a")
	if !result.aborted {
		t.Fatal("3rd request must be aborted")
	}
	if result.recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd request = %d, want 429", result.recorder.Code)
	}
	if result.recorder.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry a Retry-After header")
	}
	if body := result.recorder.Body.String(); !strings.Contains(body, "rpm_limit_exceeded") {
		t.Fatalf("body = %q, want it to contain rpm_limit_exceeded", body)
	}
}

func TestRPMMiddlewareUnlimitedKeyPassesThrough(t *testing.T) {
	s := newRPMTestServer(config.APIKeyLimits{DefaultRPM: 0})
	for i := 0; i < 50; i++ {
		if got := runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-a"); got.aborted {
			t.Fatalf("request %d aborted, want pass-through under an unlimited default", i)
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
			if got := runRPMRequest(t, s, tc.method, tc.path, "sk-a"); got.aborted {
				t.Fatalf("%s %s request %d aborted, want pass-through (exempt path)", tc.method, tc.path, i)
			}
		}
	}
	// The counted budget of 1 must still be intact.
	if got := runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-a"); got.aborted {
		t.Fatal("counted request aborted, want pass-through; exempt paths consumed the budget")
	}
	result := runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-a")
	if result.recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second counted request = %d, want 429", result.recorder.Code)
	}
}

func TestRPMMiddlewarePostToModelsPathIsCounted(t *testing.T) {
	// Gemini generation is POST /v1beta/models/*; only GETs there are metadata.
	s := newRPMTestServer(config.APIKeyLimits{DefaultRPM: 1})
	if got := runRPMRequest(t, s, http.MethodPost, "/v1beta/models/gemini-3-pro:generateContent", "sk-a"); got.aborted {
		t.Fatal("first generation aborted, want pass-through")
	}
	result := runRPMRequest(t, s, http.MethodPost, "/v1beta/models/gemini-3-pro:generateContent", "sk-a")
	if result.recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second generation = %d, want 429", result.recorder.Code)
	}
}

func TestRPMMiddlewareIsolatesKeys(t *testing.T) {
	s := newRPMTestServer(config.APIKeyLimits{DefaultRPM: 1})
	runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-a")
	if got := runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-b"); got.aborted {
		t.Fatal("sk-b aborted, want pass-through; one key's budget must not affect another")
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
	result := runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-a")
	if result.recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", result.recorder.Code)
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

// TestThrottledRequestPublishesClientMetadata proves publishRPMLimitUsage
// attaches the same client-request metadata and endpoint the normal pipeline
// attaches in GetContextWithCancel (sdk/api/handlers/handlers.go:429-452).
// Without this, client_ip, x_forwarded_for, user_agent, and endpoint arrive
// empty on the throttle record, and a shared API key hammered from one of
// several machines cannot be attributed to any particular client.
func TestThrottledRequestPublishesClientMetadata(t *testing.T) {
	captured := make(chan context.Context, 4)
	usage.RegisterNamedPlugin("rpm-limit-metadata-test", usagePluginFunc(func(ctx context.Context, _ usage.Record) {
		captured <- ctx
	}))
	defer usage.RegisterNamedPlugin("rpm-limit-metadata-test", usagePluginFunc(func(context.Context, usage.Record) {}))

	gin.SetMode(gin.TestMode)
	s := newRPMTestServer(config.APIKeyLimits{DefaultRPM: 1})

	drive := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		// A real RemoteAddr, distinct from the spoofed X-Forwarded-For below,
		// so the assertions can tell which one the record actually used.
		c.Request.RemoteAddr = "203.0.113.7:54321"
		c.Request.Header.Set("X-Forwarded-For", "198.51.100.9")
		c.Request.Header.Set("User-Agent", "claude-code/1.2.3")
		c.Set("userApiKey", "sk-a")
		s.rpmLimitMiddleware()(c)
		return recorder
	}

	drive()
	result := drive()
	if result.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", result.Code)
	}

	var recordCtx context.Context
	select {
	case recordCtx = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("no usage record published for the throttled request")
	}

	meta := logging.GetClientRequestMetadata(recordCtx)
	if meta.ClientIP != "203.0.113.7" {
		t.Fatalf("ClientIP = %q, want the host split from RemoteAddr (203.0.113.7)", meta.ClientIP)
	}
	// The security property this feature depends on: a future refactor to
	// c.ClientIP() would honor the client-supplied X-Forwarded-For below and
	// must fail this assertion.
	if meta.ClientIP == "198.51.100.9" {
		t.Fatal("ClientIP must come from RemoteAddr, not the spoofable X-Forwarded-For header")
	}
	if meta.XForwardedFor != "198.51.100.9" {
		t.Fatalf("XForwardedFor = %q, want the raw header value 198.51.100.9", meta.XForwardedFor)
	}
	if meta.UserAgent != "claude-code/1.2.3" {
		t.Fatalf("UserAgent = %q, want claude-code/1.2.3", meta.UserAgent)
	}
	if endpoint := logging.GetEndpoint(recordCtx); endpoint != "POST /v1/messages" {
		t.Fatalf("endpoint = %q, want %q", endpoint, "POST /v1/messages")
	}
}

// usagePluginFunc adapts a function to the usage.Plugin interface.
type usagePluginFunc func(context.Context, usage.Record)

func (f usagePluginFunc) HandleUsage(ctx context.Context, record usage.Record) { f(ctx, record) }

func TestRPMMiddlewareClampsRetryAfterTo60Seconds(t *testing.T) {
	// Simulate a wall-clock step backwards: pre-seed the limiter with a
	// request recorded far in the future via the limiter's own API (which
	// takes an arbitrary "now"), bypassing the middleware's time.Now(). When
	// the middleware next runs with the real, much-earlier time.Now(), Allow
	// computes retryAfter from that stale future timestamp and would return
	// a value on the order of 48 hours without the clamp.
	s := newRPMTestServer(config.APIKeyLimits{DefaultRPM: 1})
	future := time.Now().Add(48 * time.Hour)
	if ok, _ := s.rpmLimiter.Allow("sk-a", 1, future); !ok {
		t.Fatal("seed request must be allowed")
	}

	result := runRPMRequest(t, s, http.MethodPost, "/v1/messages", "sk-a")
	if result.recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("seeded key = %d, want 429", result.recorder.Code)
	}
	seconds, err := strconv.Atoi(result.recorder.Header().Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After = %q, not an integer: %v", result.recorder.Header().Get("Retry-After"), err)
	}
	if seconds > 60 {
		t.Fatalf("Retry-After = %d, want clamped to at most 60 despite the simulated clock step", seconds)
	}
}

// newRPMWiringTestServer builds a server the production way, through
// NewServer, instead of a hand-built gin engine. NewServer calls
// s.setupRoutes() (server_routes.go) and s.applyAccessConfig (which
// registers the real config-api-key access provider from cfg.APIKeys), so
// this exercises the actual .Use() registration order in server_routes.go.
// A hand-rolled engine with a manually-seeded "userApiKey" context value —
// like every other test in this file, and like the test this replaces —
// would keep passing even if someone swapped that order.
func newRPMWiringTestServer(t *testing.T, apiKey string, defaultRPM int) *Server {
	t.Helper()
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if errMkdir := os.MkdirAll(authDir, 0o700); errMkdir != nil {
		t.Fatalf("failed to create auth dir: %v", errMkdir)
	}

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			APIKeys:      []string{apiKey},
			APIKeyLimits: config.APIKeyLimits{DefaultRPM: defaultRPM},
		},
		Port:    0,
		AuthDir: authDir,
		Debug:   true,
	}

	authManager := auth.NewManager(nil, nil, nil)
	accessManager := sdkaccess.NewManager()
	configPath := filepath.Join(tmpDir, "config.yaml")
	return NewServer(cfg, authManager, accessManager, configPath)
}

// rpmWiringAuthedRequest drives one request through the real engine with a
// genuine Authorization header, the way a client would, instead of seeding
// the gin context directly.
func rpmWiringAuthedRequest(s *Server, method, path, apiKey string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	s.engine.ServeHTTP(rec, req)
	return rec
}

// TestRPMLimitEnforcedThroughRealRouting proves the real setupRoutes()
// wiring — AuthMiddleware registered before rpmLimitMiddleware — actually
// throttles traffic on each of the four proxy route groups. It builds the
// server through NewServer and drives real HTTP requests through
// s.engine.ServeHTTP with a genuine Authorization header, so "userApiKey" is
// set by AuthMiddleware exactly as it is in production, not seeded directly
// into a hand-built gin context.
//
// This is the assertion that pins the ordering: if rpmLimitMiddleware were
// registered before AuthMiddleware for any of these groups, the limiter
// would never see "userApiKey" on any request, every request would pass
// through unthrottled, and the 429 assertion below would fail for that
// group. No upstream credentials are configured; requests under the cap are
// only checked for not being 429 and may fail downstream for unrelated
// reasons (no credentials available), which is expected and irrelevant here.
func TestRPMLimitEnforcedThroughRealRouting(t *testing.T) {
	const apiKey = "sk-real"
	const limit = 2

	groups := []struct {
		name   string
		method string
		path   string
	}{
		{"v1", http.MethodPost, "/v1/messages"},
		{"openai/v1", http.MethodPost, "/openai/v1/videos"},
		{"backend-api/codex", http.MethodPost, "/backend-api/codex/responses"},
		{"v1beta", http.MethodPost, "/v1beta/models/gemini-3-pro:generateContent"},
	}

	for _, group := range groups {
		t.Run(group.name, func(t *testing.T) {
			s := newRPMWiringTestServer(t, apiKey, limit)

			for i := 0; i < limit; i++ {
				rec := rpmWiringAuthedRequest(s, group.method, group.path, apiKey)
				if rec.Code == http.StatusTooManyRequests {
					t.Fatalf("request %d = 429, want under the cap of %d to never be throttled", i, limit)
				}
			}

			rec := rpmWiringAuthedRequest(s, group.method, group.path, apiKey)
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("request past the cap = %d, want 429", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "rpm_limit_exceeded") {
				t.Fatalf("body = %q, want it to contain rpm_limit_exceeded", rec.Body.String())
			}
		})
	}
}

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
