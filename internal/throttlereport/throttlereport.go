// Package throttlereport publishes the failed-usage record and log line for a
// request rejected by the per-client-API-key RPM limit (internal/apikeylimit).
//
// It exists so the HTTP middleware (internal/api) and the WebSocket
// generation-dispatch path (sdk/api/handlers/openai) emit byte-identical
// usage records and log lines for the same kind of rejection. internal/api
// imports sdk/api/handlers/..., so sdk/api/handlers/openai cannot import
// internal/api; this package sits below both so either side can call it
// without an import cycle. It holds no state of its own and depends only on
// gin, internal/logging, and sdk/cliproxy/usage.
package throttlereport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// APIKeyHashPrefix returns the first 12 hex characters of sha256(key). CPA
// never logs API keys; this prefix matches the hash the usage sink stores, so
// a throttle log line can be joined to a human-readable alias.
func APIKeyHashPrefix(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// RequestClientIP returns the client IP from request.RemoteAddr, with the
// port split off. It deliberately does not use gin's c.ClientIP(): CPA trusts
// all proxies, so c.ClientIP() would honor a client-supplied X-Forwarded-For
// header and be spoofable — there is a prior incident behind this. Mirrors
// requestClientIP in sdk/api/handlers/handlers.go.
func RequestClientIP(request *http.Request) string {
	if request == nil {
		return ""
	}
	remoteAddr := strings.TrimSpace(request.RemoteAddr)
	if host, _, errSplit := net.SplitHostPort(remoteAddr); errSplit == nil {
		return strings.TrimSpace(host)
	}
	return remoteAddr
}

// ClampRetryAfterSeconds converts a limiter retry delay into whole seconds
// and clamps the result to [1, 60]. The window is one minute, so a correct
// retryAfter can never legitimately exceed 60s. retryAfter is derived from a
// stored timestamp and the current time with no bound against a wall-clock
// step, so if the clock steps backwards it can come out far larger than one
// window; Claude Code / Codex CLI honor a Retry-After hint verbatim, so an
// unclamped value would silence a client for far longer than the window on a
// clock glitch alone.
func ClampRetryAfterSeconds(retryAfter time.Duration) int {
	seconds := int(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	if seconds > 60 {
		seconds = 60
	}
	return seconds
}

// requestEndpoint formats "METHOD /path" the same way GetContextWithCancel
// (sdk/api/handlers/handlers.go) does, so a throttle record's endpoint field
// matches what a normal request would have carried.
func requestEndpoint(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	path := strings.TrimSpace(c.FullPath())
	if path == "" && c.Request.URL != nil {
		path = strings.TrimSpace(c.Request.URL.Path)
	}
	if path == "" {
		return ""
	}
	method := strings.TrimSpace(c.Request.Method)
	if method == "" {
		return path
	}
	return method + " " + path
}

// PublishUsage emits a failed usage record for a request rejected by the RPM
// limit. A request rejected here never reaches an executor, so the usual
// UsageReporter never runs and the request would otherwise leave no trace
// downstream — the usage database would simply show fewer requests, with
// nothing pointing at the limit. The record carries the client API key so the
// sink can attribute it and leaves model and provider empty, which the usage
// queue normalizes to "unknown".
//
// It also attaches the same endpoint and client-request metadata the normal
// pipeline attaches in GetContextWithCancel (sdk/api/handlers/handlers.go),
// so the record carries client_ip, x_forwarded_for, user_agent, and
// endpoint — without them, one API key shared across several machines could
// not be attributed to whichever client is hammering the limit.
func PublishUsage(c *gin.Context, apiKey string, limit int) {
	if c == nil || c.Request == nil {
		return
	}
	ctx := c.Request.Context()

	if endpoint := requestEndpoint(c); endpoint != "" {
		ctx = logging.WithEndpoint(ctx, endpoint)
	}
	ctx = logging.WithClientRequestMetadata(ctx, logging.ClientRequestMetadata{
		ClientIP:      RequestClientIP(c.Request),
		XForwardedFor: strings.TrimSpace(strings.Join(c.Request.Header.Values("X-Forwarded-For"), ", ")),
		UserAgent:     strings.TrimSpace(c.Request.UserAgent()),
	})

	usage.PublishRecord(ctx, usage.Record{
		APIKey:      apiKey,
		RequestedAt: time.Now(),
		Failed:      true,
		Fail: usage.Failure{
			StatusCode: http.StatusTooManyRequests,
			Body:       fmt.Sprintf(`{"error":{"code":"rpm_limit_exceeded","limit":%d}}`, limit),
		},
	})
}

// Reject logs the standard "api key rpm limit exceeded" warning and publishes
// the failed usage record (via PublishUsage) for a request rejected by the
// RPM limit, then returns the Retry-After value in whole seconds, clamped to
// [1, 60], that the caller should surface to the client. Both the HTTP
// middleware (internal/api's rpmLimitMiddleware) and the WebSocket
// generation-dispatch path (sdk/api/handlers/openai) call this single
// function so the two transports never drift into emitting different record
// shapes or log lines for the same kind of rejection.
func Reject(c *gin.Context, apiKey string, limit int, retryAfter time.Duration) int {
	seconds := ClampRetryAfterSeconds(retryAfter)
	path := ""
	if c != nil && c.Request != nil && c.Request.URL != nil {
		path = c.Request.URL.Path
	}
	log.Warnf("api key rpm limit exceeded: key_hash=%s limit=%d path=%s retry_after=%ds",
		APIKeyHashPrefix(apiKey), limit, path, seconds)
	PublishUsage(c, apiKey, limit)
	return seconds
}
