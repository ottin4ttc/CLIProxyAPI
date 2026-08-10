package throttlereport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// usagePluginFunc adapts a function to the usage.Plugin interface.
type usagePluginFunc func(context.Context, usage.Record)

func (f usagePluginFunc) HandleUsage(ctx context.Context, record usage.Record) { f(ctx, record) }

func TestAPIKeyHashPrefix(t *testing.T) {
	got := APIKeyHashPrefix("sk-a")
	if len(got) != 12 {
		t.Fatalf("len = %d, want 12", len(got))
	}
	if strings.Contains(got, "sk-a") {
		t.Fatal("the hash must not embed the key")
	}
	if got == APIKeyHashPrefix("sk-b") {
		t.Fatal("different keys must hash differently")
	}
}

func TestClampRetryAfterSeconds(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want int
	}{
		{"zero clamps up to 1", 0, 1},
		{"sub-second clamps up to 1", 500 * time.Millisecond, 1},
		{"normal value ceils", 30500 * time.Millisecond, 31},
		{"exactly 60s stays 60", 60 * time.Second, 60},
		{"beyond 60s clamps down to 60", 48 * time.Hour, 60},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClampRetryAfterSeconds(tc.in); got != tc.want {
				t.Fatalf("ClampRetryAfterSeconds(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func newThrottleTestContext(method, path string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(method, path, nil)
	return c
}

// TestPublishUsagePublishesRecordMatchingHTTPPathShape reuses the same
// assertions internal/api/rpm_limit_middleware_test.go's
// TestThrottledRequestPublishesUsageRecord makes against the HTTP path's
// former publishRPMLimitUsage, now here, so both transports are pinned to the
// same record shape: failed, 429, api key, and the rpm_limit_exceeded marker.
func TestPublishUsagePublishesRecordMatchingHTTPPathShape(t *testing.T) {
	captured := make(chan usage.Record, 4)
	usage.RegisterNamedPlugin("throttlereport-test", usagePluginFunc(func(_ context.Context, r usage.Record) {
		captured <- r
	}))
	defer usage.RegisterNamedPlugin("throttlereport-test", usagePluginFunc(func(context.Context, usage.Record) {}))

	c := newThrottleTestContext(http.MethodPost, "/v1/messages")
	PublishUsage(c, "sk-a", 40)

	var record usage.Record
	select {
	case record = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("no usage record published")
	}
	if !record.Failed {
		t.Fatal("record.Failed = false, want true")
	}
	if record.Fail.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("record.Fail.StatusCode = %d, want 429", record.Fail.StatusCode)
	}
	if record.APIKey != "sk-a" {
		t.Fatalf("record.APIKey = %q, want sk-a", record.APIKey)
	}
	if !strings.Contains(record.Fail.Body, "rpm_limit_exceeded") {
		t.Fatalf("record.Fail.Body = %q, want it to mark the throttle", record.Fail.Body)
	}
}

// TestPublishUsageAttachesClientMetadata mirrors
// TestThrottledRequestPublishesClientMetadata in
// internal/api/rpm_limit_middleware_test.go: client_ip must come from
// RemoteAddr (not the spoofable X-Forwarded-For header), and
// x_forwarded_for/user_agent/endpoint must be attached exactly as
// GetContextWithCancel (sdk/api/handlers/handlers.go) attaches them on the
// normal request pipeline.
func TestPublishUsageAttachesClientMetadata(t *testing.T) {
	captured := make(chan context.Context, 4)
	usage.RegisterNamedPlugin("throttlereport-metadata-test", usagePluginFunc(func(ctx context.Context, _ usage.Record) {
		captured <- ctx
	}))
	defer usage.RegisterNamedPlugin("throttlereport-metadata-test", usagePluginFunc(func(context.Context, usage.Record) {}))

	c := newThrottleTestContext(http.MethodPost, "/v1/messages")
	c.Request.RemoteAddr = "203.0.113.7:54321"
	c.Request.Header.Set("X-Forwarded-For", "198.51.100.9")
	c.Request.Header.Set("User-Agent", "claude-code/1.2.3")

	PublishUsage(c, "sk-a", 40)

	var recordCtx context.Context
	select {
	case recordCtx = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("no usage record published")
	}

	meta := logging.GetClientRequestMetadata(recordCtx)
	if meta.ClientIP != "203.0.113.7" {
		t.Fatalf("ClientIP = %q, want the host split from RemoteAddr (203.0.113.7)", meta.ClientIP)
	}
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

// TestRejectReturnsClampedSecondsAndPublishes proves the single Reject
// entrypoint both callers (HTTP middleware and WebSocket path) use performs
// the clamp and publishes the record in one call.
func TestRejectReturnsClampedSecondsAndPublishes(t *testing.T) {
	captured := make(chan usage.Record, 4)
	usage.RegisterNamedPlugin("throttlereport-reject-test", usagePluginFunc(func(_ context.Context, r usage.Record) {
		captured <- r
	}))
	defer usage.RegisterNamedPlugin("throttlereport-reject-test", usagePluginFunc(func(context.Context, usage.Record) {}))

	c := newThrottleTestContext(http.MethodGet, "/v1/responses")
	seconds := Reject(c, "sk-a", 1, 48*time.Hour)
	if seconds != 60 {
		t.Fatalf("Reject seconds = %d, want clamped to 60", seconds)
	}

	select {
	case record := <-captured:
		if !record.Failed || record.APIKey != "sk-a" {
			t.Fatalf("record = %+v, want a failed record for sk-a", record)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Reject did not publish a usage record")
	}
}

func TestRequestClientIPHandlesMissingPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = "203.0.113.5"
	if got := RequestClientIP(req); got != "203.0.113.5" {
		t.Fatalf("RequestClientIP = %q, want 203.0.113.5 (RemoteAddr with no port falls through as-is)", got)
	}
}

func TestRequestClientIPNilRequest(t *testing.T) {
	if got := RequestClientIP(nil); got != "" {
		t.Fatalf("RequestClientIP(nil) = %q, want empty", got)
	}
}
