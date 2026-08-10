package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	codexlive "github.com/router-for-me/CLIProxyAPI/v7/internal/client/codex/live"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/safemode"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

var corsExposedResponseHeaders = []string{
	logging.CPATraceIDHeader,
	"X-CPA-VERSION",
	"X-CPA-COMMIT",
	"X-CPA-BUILD-DATE",
	"X-CPA-SUPPORT-PLUGIN",
	"X-CPA-HOME-VERSION",
	"X-CPA-HOME-BUILD-DATE",
	"X-SERVER-VERSION",
	"X-SERVER-BUILD-DATE",
	"Location",
	// Retry-After is set by rpmLimitMiddleware on 429s; without exposing it a
	// browser client cannot read it and cannot back off correctly.
	"Retry-After",
	"X-Request-Id",
	"OpenAI-Request-Id",
}

var corsExposedResponseHeadersJoined = strings.Join(corsExposedResponseHeaders, ", ")

const (
	exampleAPIKeyManagementPath = "/management.html"
	exampleAPIKeyManagementURL  = "/management.html?safe-mode=configure"
)

func (s *Server) homeHeartbeatMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || s.cfg == nil || !s.cfg.Home.Enabled {
			c.Next()
			return
		}
		if c != nil && c.Request != nil {
			path := c.Request.URL.Path
			if strings.HasPrefix(path, "/v0/management/") || path == "/v0/management" || strings.HasPrefix(path, "/v0/resource/plugins/") || path == "/management.html" {
				c.Next()
				return
			}
		}
		client := home.Current()
		if client == nil || !client.HeartbeatOK() {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.Next()
	}
}

func (s *Server) exampleAPIKeySafeModeRequired(cfg *config.Config) bool {
	return s != nil && s.exampleAPIKeySafeModeEnabled && cfg != nil && safemode.HasExampleAPIKeys(cfg.APIKeys)
}

func (s *Server) exampleAPIKeySafeModeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || !s.exampleAPIKeySafeModeActive.Load() || c == nil || c.Request == nil || c.Request.URL == nil {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if path == exampleAPIKeyManagementPath && c.Query("safe-mode") == "configure" {
			c.Next()
			return
		}
		if (path == "/" || path == exampleAPIKeyManagementPath) && (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) {
			s.serveExampleAPIKeyWarningPage(c)
			return
		}
		if !isExampleAPIKeySafeModeProxyPath(path) {
			c.Next()
			return
		}

		c.Header("X-CPA-SAFE-MODE", "example-api-key")
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error":   "unsafe_example_api_key",
			"message": "Proxy API endpoints are disabled because api-keys contains template values. Open /management.html?safe-mode=configure, update api-keys in Management, then retry.",
		})
	}
}

func (s *Server) serveExampleAPIKeyWarningPage(c *gin.Context) {
	cfg := s.cfg
	var keys []string
	if cfg != nil {
		keys = safemode.ExampleAPIKeys(cfg.APIKeys)
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		c.Abort()
		return
	}
	c.String(http.StatusOK, safemode.ExampleAPIKeyWarningPageHTML(keys, exampleAPIKeyManagementURL))
	c.Abort()
}

func isExampleAPIKeySafeModeProxyPath(path string) bool {
	switch {
	case path == "/v1" || strings.HasPrefix(path, "/v1/"):
		return true
	case path == "/v1beta" || strings.HasPrefix(path, "/v1beta/"):
		return true
	case path == "/openai/v1" || strings.HasPrefix(path, "/openai/v1/"):
		return true
	case path == "/backend-api/codex" || strings.HasPrefix(path, "/backend-api/codex/"):
		return true
	default:
		return false
	}
}

// corsMiddleware returns a Gin middleware handler that adds CORS headers
// to every response, allowing cross-origin requests.
//
// Returns:
//   - gin.HandlerFunc: The CORS middleware handler
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "*")
		c.Header("Access-Control-Expose-Headers", corsExposedResponseHeadersJoined)

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// AuthMiddleware returns a Gin middleware handler that authenticates requests
// using the configured authentication providers. When no providers are available,
// it allows all requests (legacy behaviour).
func AuthMiddleware(manager *sdkaccess.Manager) gin.HandlerFunc {
	return accessAuthMiddleware(manager, false)
}

func realtimeStandardAuthMiddleware(manager *sdkaccess.Manager) gin.HandlerFunc {
	return accessAuthMiddleware(manager, true)
}

func accessAuthMiddleware(manager *sdkaccess.Manager, realtimeError bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if manager == nil {
			c.Next()
			return
		}

		result, err := manager.Authenticate(c.Request.Context(), c.Request)
		if err == nil {
			if result != nil {
				c.Set("userApiKey", result.Principal)
				c.Set("accessProvider", result.Provider)
				if len(result.Metadata) > 0 {
					c.Set("accessMetadata", result.Metadata)
				}
			}
			c.Next()
			return
		}

		statusCode := err.HTTPStatusCode()
		if statusCode >= http.StatusInternalServerError {
			log.Errorf("authentication middleware error: %v", err)
		}
		if realtimeError {
			errorType := "authentication_error"
			code := "invalid_api_key"
			if statusCode >= http.StatusInternalServerError {
				errorType = "server_error"
				code = "authentication_service_error"
			}
			c.AbortWithStatusJSON(statusCode, gin.H{"error": gin.H{
				"message": err.Message,
				"type":    errorType,
				"param":   nil,
				"code":    code,
			}})
			return
		}
		c.AbortWithStatusJSON(statusCode, gin.H{"error": err.Message})
	}
}

func realtimeAuthMiddleware(manager *sdkaccess.Manager, handler *codexlive.Handler) gin.HandlerFunc {
	fallback := realtimeStandardAuthMiddleware(manager)
	return func(c *gin.Context) {
		authorization, matched, errAuthenticate := handler.AuthenticateClientSecret(c.Request)
		if !matched {
			fallback(c)
			return
		}
		if errAuthenticate != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{
				"message": errAuthenticate.Error(),
				"type":    "invalid_request_error",
				"param":   nil,
				"code":    "invalid_realtime_client_secret",
			}})
			return
		}
		principal := authorization.IssuerPrincipal
		if principal == "" {
			principal = authorization.Principal
		}
		provider := authorization.IssuerProvider
		if provider == "" {
			provider = "realtime-client-secret"
		}
		c.Set("userApiKey", principal)
		c.Set("accessProvider", provider)
		c.Set(codexlive.ClientSecretSessionContextKey, authorization.Session)
		c.Set(codexlive.ClientSecretPrincipalContextKey, authorization.Principal)
		c.Next()
	}
}

// rpmLimitExemptPath reports whether a proxy path is excluded from RPM
// accounting: model metadata, POST /v1/messages/count_tokens, and the sideband
// companion channels of a call that was already counted when it was opened.
// Counting a sideband would charge one call twice.
//
// count_tokens is exempt because it produces no usage_events row and was
// therefore excluded from the usage_events measurement the default limit was
// calibrated against — not because it is "local". For Anthropic-family
// credentials it is a real upstream HTTP call to api.anthropic.com that
// consumes a credential from the shared pool (see
// shouldUseClaudeUpstreamTokenCount in claude_executor_tokens.go). This is a
// known uncounted path: a client can drive real, paid upstream traffic
// through it without ever showing up in this middleware's count.
func rpmLimitExemptPath(method, path string) bool {
	switch path {
	case "/v1/models", "/v1beta/models", "/v1/messages/count_tokens", "/v1/realtime":
		return true
	}
	if method != http.MethodGet {
		return false
	}
	switch {
	case strings.HasPrefix(path, "/v1beta/models/"):
		// All Gemini generation on this prefix is POST; GET is metadata.
		return true
	case strings.HasPrefix(path, "/v1/live/"):
		return true
	case strings.HasPrefix(path, "/v1/realtime/calls/"):
		return true
	}
	return false
}

// apiKeyHashPrefix returns the first 12 hex characters of sha256(key). CPA never
// logs API keys; this prefix matches the hash the usage sink stores, so a
// throttle log line can be joined to a human-readable alias.
func apiKeyHashPrefix(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// requestClientIP returns the client IP from request.RemoteAddr, with the port
// split off. It deliberately does not use gin's c.ClientIP(): this server
// trusts all proxies, so c.ClientIP() would honor a client-supplied
// X-Forwarded-For header and be spoofable — there is a prior incident behind
// this. Mirrors requestClientIP in sdk/api/handlers/handlers.go.
func requestClientIP(request *http.Request) string {
	if request == nil {
		return ""
	}
	remoteAddr := strings.TrimSpace(request.RemoteAddr)
	if host, _, errSplit := net.SplitHostPort(remoteAddr); errSplit == nil {
		return strings.TrimSpace(host)
	}
	return remoteAddr
}

// publishRPMLimitUsage emits a failed usage record for a throttled request.
//
// A request rejected here never reaches an executor, so the usual UsageReporter
// never runs and the request would otherwise leave no trace downstream — the
// usage database would simply show fewer requests, with nothing pointing at the
// limit. The record carries the client API key so the sink can attribute it and
// leaves model and provider empty, which the usage queue normalizes to
// "unknown"; the request body is never parsed on this path.
//
// It also attaches the same endpoint and client-request metadata the normal
// pipeline attaches in GetContextWithCancel (sdk/api/handlers/handlers.go), so
// the record carries client_ip, x_forwarded_for, user_agent, and endpoint —
// without them, one API key shared across several machines could not be
// attributed to whichever client is hammering the limit.
func publishRPMLimitUsage(c *gin.Context, apiKey string, limit int) {
	ctx := c.Request.Context()

	endpoint := ""
	path := strings.TrimSpace(c.FullPath())
	if path == "" && c.Request.URL != nil {
		path = strings.TrimSpace(c.Request.URL.Path)
	}
	if path != "" {
		method := strings.TrimSpace(c.Request.Method)
		if method != "" {
			endpoint = method + " " + path
		} else {
			endpoint = path
		}
	}
	if endpoint != "" {
		ctx = logging.WithEndpoint(ctx, endpoint)
	}
	ctx = logging.WithClientRequestMetadata(ctx, logging.ClientRequestMetadata{
		ClientIP:      requestClientIP(c.Request),
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

// rpmLimitMiddleware enforces the per-client-API-key requests-per-minute cap.
// It must be registered after AuthMiddleware, which is what sets "userApiKey".
// Unlike a concurrency limit it holds nothing for the request's lifetime, so it
// never interacts with SSE or WebSocket duration.
//
// It counts requests, not generations: GET /v1/responses and
// GET /backend-api/codex/responses are WebSocket upgrades, and a WebSocket
// connection consumes exactly one unit of budget at handshake regardless of
// how many generations it later carries over that socket, each of which
// writes its own usage_events row. RPM figures derived from usage_events
// therefore overstate what this middleware sees for WebSocket-transport
// clients; anyone re-calibrating default-rpm from usage_events must split by
// endpoint (GET vs POST) first. See the design doc for the measured impact.
func (s *Server) rpmLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || s.rpmLimiter == nil || s.cfg == nil || c.Request == nil || c.Request.URL == nil {
			c.Next()
			return
		}
		if rpmLimitExemptPath(c.Request.Method, c.Request.URL.Path) {
			c.Next()
			return
		}
		// userApiKey holds the access provider's Principal, not necessarily a
		// configured API key: the built-in config-api-key provider sets it to the
		// real key, but a plugin frontend-auth provider (see
		// pluginhost/adapters_auth.go) may return an arbitrary constant Principal,
		// in which case all of that plugin's traffic shares one bucket here.
		value, exists := c.Get("userApiKey")
		if !exists || value == nil {
			c.Next()
			return
		}
		key := fmt.Sprint(value)
		if key == "" {
			c.Next()
			return
		}
		limit := s.cfg.RPMLimitForAPIKey(key)
		ok, retryAfter := s.rpmLimiter.Allow(key, limit, time.Now())
		if ok {
			c.Next()
			return
		}
		seconds := int(math.Ceil(retryAfter.Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		// The window is one minute, so a correct retryAfter can never
		// legitimately exceed 60s. retryAfter is derived from a stored
		// timestamp and the current now (Allow provides no bound against a
		// wall-clock step), so if the clock steps backwards it can come out
		// far larger than one window. Claude Code / Codex CLI honor this
		// header verbatim, so an unclamped value would silence a client for
		// far longer than the window on a clock glitch alone.
		if seconds > 60 {
			seconds = 60
		}
		log.Warnf("api key rpm limit exceeded: key_hash=%s limit=%d path=%s retry_after=%ds",
			apiKeyHashPrefix(key), limit, c.Request.URL.Path, seconds)
		publishRPMLimitUsage(c, key, limit)
		c.Header("Retry-After", strconv.Itoa(seconds))
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{
			"message": "Request rate limit exceeded for this API key.",
			"type":    "rate_limit_error",
			"code":    "rpm_limit_exceeded",
		}})
	}
}
