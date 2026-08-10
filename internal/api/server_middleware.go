package api

import (
	"fmt"
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
	"github.com/router-for-me/CLIProxyAPI/v7/internal/throttlereport"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
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
// accounting: model metadata, POST /v1/messages/count_tokens, the sideband
// companion channels of a call that was already counted when it was opened,
// and the two WebSocket handshake paths whose generations are counted
// individually at dispatch time instead of once at handshake (see
// "WebSocket accounting" below). Counting a sideband, or the handshake in
// addition to its generations, would charge one call twice.
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
	case path == "/v1/responses":
		// WebSocket upgrade; see openai_responses_websocket.go, which counts
		// each generation dispatched over the socket instead.
		return true
	case path == "/backend-api/codex/responses":
		// Same WebSocket upgrade as /v1/responses, reached through the Codex
		// direct route.
		return true
	}
	return false
}

// rpmLimitMiddleware enforces the per-client-API-key requests-per-minute cap.
// It must be registered after AuthMiddleware, which is what sets "userApiKey".
// Unlike a concurrency limit it holds nothing for the request's lifetime, so it
// never interacts with SSE or WebSocket duration.
//
// It counts requests, one unit per HTTP request. GET /v1/responses and
// GET /backend-api/codex/responses are WebSocket upgrades and are exempt here
// (rpmLimitExemptPath): the handshake itself no longer counts, and each
// generation dispatched over the socket instead consumes one unit directly
// against the shared apikeylimit.Default() limiter in the WebSocket read loop
// (sdk/api/handlers/openai/openai_responses_websocket.go), the same limiter
// this middleware uses. One unit therefore always means one generation on
// both transports, matching how usage_events counts a generation — figures
// derived from usage_events are directly comparable to what this limiter
// counts, on either transport.
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
		seconds := throttlereport.Reject(c, key, limit, retryAfter)
		c.Header("Retry-After", strconv.Itoa(seconds))
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{
			"message": "Request rate limit exceeded for this API key.",
			"type":    "rate_limit_error",
			"code":    "rpm_limit_exceeded",
		}})
	}
}
