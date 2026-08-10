package openai

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/apikeylimit"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/throttlereport"
)

// responsesWebsocketRPMDecision reports whether the next generation dispatch
// on an established /v1/responses (or /backend-api/codex/responses)
// WebSocket may proceed under the client API key's requests-per-minute cap.
//
// The HTTP path (internal/api's rpmLimitMiddleware) charges one unit per HTTP
// request; over a WebSocket the handshake itself is exempt
// (rpmLimitExemptPath in internal/api/server_middleware.go), so this must be
// called once per generation dispatch instead — one unit per
// h.ExecuteStreamWithAuthManager call, matching how usage_events counts a
// generation.
//
// limiter == nil or apiKey == "" always allows, matching Limiter.Allow's own
// nil-receiver/empty-key short-circuit, so this stays a byte-identical no-op
// when the feature is unconfigured or the socket has no attributable key.
// limit is resolved by the caller on every call (never cached on the
// connection), so a config hot-reload of the per-key RPM cap takes effect on
// the very next turn.
func responsesWebsocketRPMDecision(limiter *apikeylimit.Limiter, apiKey string, limit int, now time.Time) (bool, time.Duration) {
	if limiter == nil || apiKey == "" {
		return true, 0
	}
	return limiter.Allow(apiKey, limit, now)
}

// responsesWebsocketAPIKey reads the client API key AuthMiddleware set on the
// gin context, the same "userApiKey" value rpmLimitMiddleware reads on the
// HTTP path. It mirrors that middleware's fmt.Sprint(value) conversion
// exactly, because userApiKey holds the access provider's Principal, not
// necessarily a configured API key, and is not guaranteed to be a string.
func responsesWebsocketAPIKey(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, exists := c.Get("userApiKey")
	if !exists || value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

// responsesWebsocketRPMLimitReject evaluates the per-API-key RPM limit for
// the next generation dispatch on this socket. When the key is over its cap
// it reports the same throttlereport usage record and log line the HTTP
// middleware emits for a throttled request, and returns a WebSocket-shaped
// error the caller should write to the client before continuing the read
// loop without dispatching. A nil return means the caller may proceed.
func (h *OpenAIResponsesAPIHandler) responsesWebsocketRPMLimitReject(c *gin.Context) *interfaces.ErrorMessage {
	if h == nil || h.Cfg == nil {
		return nil
	}
	key := responsesWebsocketAPIKey(c)
	if key == "" {
		return nil
	}
	limit := h.Cfg.RPMLimitForAPIKey(key)
	ok, retryAfter := responsesWebsocketRPMDecision(apikeylimit.Default(), key, limit, time.Now())
	if ok {
		return nil
	}
	throttlereport.Reject(c, key, limit, retryAfter)
	return responsesWebsocketRPMLimitExceededError()
}

// responsesWebsocketRPMLimitExceededError mirrors the HTTP 429 body
// rpmLimitMiddleware returns, including the rpm_limit_exceeded code, so
// downstream readers can filter a throttled request identically on either
// transport. The connection itself is left open: the client should retry the
// same turn shortly, not replay the whole session on a new socket.
func responsesWebsocketRPMLimitExceededError() *interfaces.ErrorMessage {
	return &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error: errors.New(
			`{"error":{"message":"Request rate limit exceeded for this API key.","type":"rate_limit_error","code":"rpm_limit_exceeded"}}`,
		),
	}
}
