package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	"golang.org/x/crypto/bcrypt"
)

func TestAuthenticateManagementKey_RemembersVerifiedKeyPerSecretHash(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("test-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}
	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = string(hash)
	h := &Handler{cfg: cfg, failedAttempts: make(map[string]*attemptInfo)}

	if allowed, _, msg := h.AuthenticateManagementKey("127.0.0.1", true, "test-secret"); !allowed {
		t.Fatalf("expected correct key to pass bcrypt: %q", msg)
	}
	if h.verifiedKey.Load() == nil {
		t.Fatalf("expected the verified key to be remembered")
	}
	if allowed, _, msg := h.AuthenticateManagementKey("127.0.0.1", true, "test-secret"); !allowed {
		t.Fatalf("expected remembered key to pass: %q", msg)
	}
	if allowed, _, _ := h.AuthenticateManagementKey("127.0.0.1", true, "wrong-secret"); allowed {
		t.Fatalf("expected a different key to be rejected while one is remembered")
	}

	// A rotated hash must not honour the remembered key.
	rotated, err := bcrypt.GenerateFromPassword([]byte("rotated-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash rotated secret: %v", err)
	}
	cfg.RemoteManagement.SecretKey = string(rotated)
	if allowed, _, _ := h.AuthenticateManagementKey("127.0.0.1", true, "test-secret"); allowed {
		t.Fatalf("expected the old key to be rejected after rotation")
	}
	if allowed, _, msg := h.AuthenticateManagementKey("127.0.0.1", true, "rotated-secret"); !allowed {
		t.Fatalf("expected the rotated key to pass: %q", msg)
	}
	if cached := h.verifiedKey.Load(); cached == nil || cached.secretHash != string(rotated) {
		t.Fatalf("expected the remembered key to follow the rotated hash")
	}
}

func TestAuthenticateManagementKey_LocalhostIPBan_BlocksCorrectKeyDuringBan(t *testing.T) {
	h := &Handler{
		cfg:            &config.Config{},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
	}

	for i := 0; i < 5; i++ {
		allowed, statusCode, errMsg := h.AuthenticateManagementKey("127.0.0.1", true, "wrong-secret")
		if allowed {
			t.Fatalf("expected auth to be denied at attempt %d", i+1)
		}
		if statusCode != http.StatusUnauthorized || errMsg != "invalid management key" {
			t.Fatalf("unexpected auth failure at attempt %d: status=%d msg=%q", i+1, statusCode, errMsg)
		}
	}

	allowed, statusCode, errMsg := h.AuthenticateManagementKey("127.0.0.1", true, "test-secret")
	if allowed {
		t.Fatalf("expected correct key to be denied while banned")
	}
	if statusCode != http.StatusForbidden {
		t.Fatalf("expected forbidden status while banned, got %d", statusCode)
	}
	if !strings.HasPrefix(errMsg, "IP banned due to too many failed attempts. Try again in") {
		t.Fatalf("unexpected banned message: %q", errMsg)
	}
}

func TestMiddlewareSetsSupportPluginHeader(t *testing.T) {

	h := &Handler{
		cfg:            &config.Config{},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
	}
	middleware := h.Middleware()

	t.Run("invalid key", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		c.Request.RemoteAddr = "127.0.0.1:12345"
		c.Request.Header.Set("X-Management-Key", "wrong-secret")

		middleware(c)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})

	t.Run("valid key", func(t *testing.T) {
		engine := gin.New()
		engine.GET("/v0/management/config", middleware, func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Management-Key", "test-secret")
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})
}
