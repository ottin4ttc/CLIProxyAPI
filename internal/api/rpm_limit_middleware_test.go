package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	configaccess "github.com/router-for-me/CLIProxyAPI/v7/internal/access/config_access"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/apikeylimit"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
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

// TestRPMMiddlewareWiredAfterAuthMiddleware proves the middleware behaves
// correctly only when registered the way server_routes.go registers it —
// AuthMiddleware first, rpmLimitMiddleware second — using a real gin engine,
// the real config-api-key access provider, and real HTTP requests, instead of
// calling s.rpmLimitMiddleware() directly with a hand-seeded "userApiKey"
// like every other test in this file. Every other test would still pass if
// someone moved the registration order in server_routes.go; this one would not.
func TestRPMMiddlewareWiredAfterAuthMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newEngine := func(reverseOrder bool) *gin.Engine {
		cfg := &config.Config{}
		cfg.APIKeys = []string{"sk-real"}
		cfg.APIKeyLimits = config.APIKeyLimits{DefaultRPM: 1}

		// Register the real built-in config-api-key provider, the way
		// sdk/cliproxy/builder.go does, so AuthMiddleware sets a real
		// "userApiKey" from the Authorization header instead of one seeded
		// directly into the gin context.
		configaccess.Register(&cfg.SDKConfig)
		t.Cleanup(func() { sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey) })
		accessManager := sdkaccess.NewManager()
		accessManager.SetProviders(sdkaccess.RegisteredProviders())

		s := &Server{cfg: cfg, rpmLimiter: apikeylimit.New(), accessManager: accessManager}

		engine := gin.New()
		group := engine.Group("/v1")
		if reverseOrder {
			// The wrong order: rpmLimitMiddleware runs before AuthMiddleware
			// has had a chance to set "userApiKey", so it can never find a
			// key and must always pass through, no matter the limit.
			group.Use(s.rpmLimitMiddleware())
			group.Use(AuthMiddleware(accessManager))
		} else {
			group.Use(AuthMiddleware(accessManager))
			group.Use(s.rpmLimitMiddleware())
		}
		group.POST("/messages", func(c *gin.Context) { c.Status(http.StatusOK) })
		return engine
	}

	authedRequest := func(engine *gin.Engine) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("Authorization", "Bearer sk-real")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}

	t.Run("correct order: authenticated over-limit request is throttled", func(t *testing.T) {
		engine := newEngine(false)
		if got := authedRequest(engine).Code; got != http.StatusOK {
			t.Fatalf("first request = %d, want 200", got)
		}
		if got := authedRequest(engine).Code; got != http.StatusTooManyRequests {
			t.Fatalf("second request = %d, want 429 when registered after AuthMiddleware", got)
		}
	})

	t.Run("reversed order: limiter never fires because userApiKey is not yet set", func(t *testing.T) {
		engine := newEngine(true)
		for i := 0; i < 5; i++ {
			if got := authedRequest(engine).Code; got != http.StatusOK {
				t.Fatalf("request %d = %d, want 200; rpmLimitMiddleware before AuthMiddleware must never see userApiKey and so must never throttle", i, got)
			}
		}
	})
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
