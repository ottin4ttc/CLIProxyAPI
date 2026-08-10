package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/apikeylimit"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

// --- responsesWebsocketRPMDecision: unit tests, no live socket ---

func TestResponsesWebsocketRPMDecisionUnderLimitProceeds(t *testing.T) {
	limiter := apikeylimit.New()
	now := time.Now()
	ok, retryAfter := responsesWebsocketRPMDecision(limiter, "sk-a", 2, now)
	if !ok {
		t.Fatal("first generation under limit=2 rejected, want proceed")
	}
	if retryAfter != 0 {
		t.Fatalf("retryAfter = %v, want 0 when allowed", retryAfter)
	}
}

func TestResponsesWebsocketRPMDecisionOverLimitRejectsWithPositiveRetryDelay(t *testing.T) {
	limiter := apikeylimit.New()
	now := time.Now()
	if ok, _ := responsesWebsocketRPMDecision(limiter, "sk-a", 1, now); !ok {
		t.Fatal("first generation under limit=1 rejected, want proceed")
	}
	ok, retryAfter := responsesWebsocketRPMDecision(limiter, "sk-a", 1, now)
	if ok {
		t.Fatal("second generation over limit=1 proceeded, want rejected")
	}
	if retryAfter <= 0 {
		t.Fatalf("retryAfter = %v, want a positive delay", retryAfter)
	}
}

func TestResponsesWebsocketRPMDecisionUnlimitedAlwaysProceeds(t *testing.T) {
	limiter := apikeylimit.New()
	now := time.Now()
	for i := 0; i < 500; i++ {
		if ok, _ := responsesWebsocketRPMDecision(limiter, "sk-a", 0, now); !ok {
			t.Fatalf("generation %d rejected under limit=0 (unlimited), want proceed", i)
		}
	}
}

func TestResponsesWebsocketRPMDecisionNilLimiterAlwaysProceeds(t *testing.T) {
	// Matches Limiter.Allow's own nil-receiver short-circuit, so an
	// unconfigured/uninitialized limiter never blocks a generation.
	if ok, _ := responsesWebsocketRPMDecision(nil, "sk-a", 1, time.Now()); !ok {
		t.Fatal("nil limiter rejected a generation, want proceed")
	}
}

func TestResponsesWebsocketRPMDecisionEmptyKeyAlwaysProceeds(t *testing.T) {
	limiter := apikeylimit.New()
	now := time.Now()
	for i := 0; i < 5; i++ {
		if ok, _ := responsesWebsocketRPMDecision(limiter, "", 1, now); !ok {
			t.Fatalf("generation %d with an empty key rejected, want proceed (no attributable key to charge)", i)
		}
	}
}

// --- responsesWebsocketAPIKey ---

func TestResponsesWebsocketAPIKeyReadsUserApiKeyFromGinContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if got := responsesWebsocketAPIKey(c); got != "" {
		t.Fatalf("key = %q before userApiKey is set, want empty", got)
	}
	c.Set("userApiKey", "sk-context")
	if got := responsesWebsocketAPIKey(c); got != "sk-context" {
		t.Fatalf("key = %q, want sk-context", got)
	}
}

// --- responsesWebsocketRPMLimitExceededError ---

func TestResponsesWebsocketRPMLimitExceededErrorMatchesHTTPShape(t *testing.T) {
	errMsg := responsesWebsocketRPMLimitExceededError()
	if errMsg.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("StatusCode = %d, want 429", errMsg.StatusCode)
	}
	body := errMsg.Error.Error()
	if !strings.Contains(body, "rpm_limit_exceeded") {
		t.Fatalf("body = %q, want it to contain rpm_limit_exceeded", body)
	}
	if !strings.Contains(body, "rate_limit_error") {
		t.Fatalf("body = %q, want the same type the HTTP path uses", body)
	}
}

// usagePluginFunc adapts a function to the usage.Plugin interface, mirroring
// the identically-named helper in internal/api/rpm_limit_middleware_test.go.
type usagePluginFunc func(context.Context, usage.Record)

func (f usagePluginFunc) HandleUsage(ctx context.Context, record usage.Record) { f(ctx, record) }

// --- end-to-end: real WebSocket round trip through h.ResponsesWebsocket ---

// rpmTestExecutor completes every ExecuteStream call immediately with a
// synthetic response.completed event, so a turn that is allowed to proceed
// dispatches and finishes without needing a real upstream.
type rpmTestExecutor struct {
	calls int
}

func (*rpmTestExecutor) Identifier() string { return "codex" }

func (*rpmTestExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *rpmTestExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.calls++
	if lifecycle, ok := opts.ExecutionLifecycle.(interface{ Retain() }); ok {
		lifecycle.Retain()
	}
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"rpm-test-response","output":[]}}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (*rpmTestExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, errors.New("not implemented")
}

func (*rpmTestExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (*rpmTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

// TestResponsesWebsocketRPMLimitRejectsSecondGenerationButKeepsSocketOpen
// drives two real generations over one live WebSocket connection with
// default-rpm=1. It proves, against the actual read loop in
// ResponsesWebsocket (not just the decision function): the first generation
// dispatches and completes normally, the second is rejected with a
// "type":"error" frame carrying code=rpm_limit_exceeded and status=429
// instead of reaching the executor, the connection is never closed by the
// rejection (a third message is still readable), and a failed usage record
// is published for the rejected turn — matching the HTTP path's
// observability guarantee.
func TestResponsesWebsocketRPMLimitRejectsSecondGenerationButKeepsSocketOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)

	captured := make(chan usage.Record, 4)
	usage.RegisterNamedPlugin("responses-websocket-rpm-test", usagePluginFunc(func(_ context.Context, r usage.Record) {
		captured <- r
	}))
	defer usage.RegisterNamedPlugin("responses-websocket-rpm-test", usagePluginFunc(func(context.Context, usage.Record) {}))

	executor := &rpmTestExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "auth-rpm-ws", Provider: "codex", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	cfg := &sdkconfig.SDKConfig{APIKeyLimits: internalconfig.APIKeyLimits{DefaultRPM: 1}}
	base := handlers.NewBaseAPIHandlers(cfg, manager)
	h := NewOpenAIResponsesAPIHandler(base)

	router := gin.New()
	router.GET("/v1/responses/ws", func(c *gin.Context) {
		// AuthMiddleware's real job: attribute this socket to a client API key.
		c.Set("userApiKey", "sk-ws-rpm-test")
		h.ResponsesWebsocket(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses/ws"
	conn, _, errDial := websocket.DefaultDialer.Dial(wsURL, nil)
	if errDial != nil {
		t.Fatalf("dial websocket: %v", errDial)
	}
	defer func() { _ = conn.Close() }()

	request := []byte(`{"type":"response.create","model":"test-model","input":[]}`)

	// First generation: under the cap of 1, must dispatch and complete.
	if errWrite := conn.WriteMessage(websocket.TextMessage, request); errWrite != nil {
		t.Fatalf("write first request: %v", errWrite)
	}
	_, firstPayload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read first response: %v", errRead)
	}
	if got := gjson.GetBytes(firstPayload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("first response type = %q, want %q: %s", got, wsEventTypeCompleted, firstPayload)
	}

	// Second generation: over the cap, must be rejected without dispatch.
	if errWrite := conn.WriteMessage(websocket.TextMessage, request); errWrite != nil {
		t.Fatalf("write second request: %v", errWrite)
	}
	_, secondPayload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read second response: %v", errRead)
	}
	if got := gjson.GetBytes(secondPayload, "type").String(); got != wsEventTypeError {
		t.Fatalf("second response type = %q, want %q (rejected): %s", got, wsEventTypeError, secondPayload)
	}
	if got := gjson.GetBytes(secondPayload, "status").Int(); got != http.StatusTooManyRequests {
		t.Fatalf("second response status = %d, want 429: %s", got, secondPayload)
	}
	if got := gjson.GetBytes(secondPayload, "error.code").String(); got != "rpm_limit_exceeded" {
		t.Fatalf("second response error.code = %q, want rpm_limit_exceeded: %s", got, secondPayload)
	}

	// Third message on the SAME connection: proves the rejection above did not
	// close the socket. It is also rejected (still within the same 60s
	// window), which is itself further proof the connection stayed alive and
	// serving the read loop rather than being torn down.
	if errWrite := conn.WriteMessage(websocket.TextMessage, request); errWrite != nil {
		t.Fatalf("write third request (proves socket still open): %v", errWrite)
	}
	_, thirdPayload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read third response (socket should still be open): %v", errRead)
	}
	if got := gjson.GetBytes(thirdPayload, "type").String(); got != wsEventTypeError {
		t.Fatalf("third response type = %q, want %q: %s", got, wsEventTypeError, thirdPayload)
	}

	if executor.calls != 1 {
		t.Fatalf("executor dispatched %d times, want exactly 1 (only the first, allowed generation)", executor.calls)
	}

	var record usage.Record
	select {
	case record = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("no usage record published for the rejected WebSocket generation")
	}
	if !record.Failed {
		t.Fatal("record.Failed = false, want true")
	}
	if record.Fail.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("record.Fail.StatusCode = %d, want 429", record.Fail.StatusCode)
	}
	if record.APIKey != "sk-ws-rpm-test" {
		t.Fatalf("record.APIKey = %q, want sk-ws-rpm-test", record.APIKey)
	}
	if !strings.Contains(record.Fail.Body, "rpm_limit_exceeded") {
		t.Fatalf("record.Fail.Body = %q, want it to mark the throttle", record.Fail.Body)
	}
}
