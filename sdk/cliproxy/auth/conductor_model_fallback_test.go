package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// codexTierExecutor fails the primary tier with the given cause and succeeds on
// any other model, recording every model it was asked to run.
type codexTierExecutor struct {
	primary string
	cause   string
	seen    []string
}

func (e *codexTierExecutor) Identifier() string { return "codex" }

func (e *codexTierExecutor) Execute(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.seen = append(e.seen, req.Model)
	if req.Model == e.primary {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "overloaded", Cause: e.cause}
	}
	return cliproxyexecutor.Response{Payload: []byte(req.Model)}, nil
}

func (e *codexTierExecutor) ExecuteStream(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.seen = append(e.seen, req.Model)
	if req.Model == e.primary {
		return nil, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "overloaded", Cause: e.cause}
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte(req.Model)}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *codexTierExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (e *codexTierExecutor) CountTokens(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.seen = append(e.seen, req.Model)
	if req.Model == e.primary {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "overloaded", Cause: e.cause}
	}
	return cliproxyexecutor.Response{Payload: []byte("count:" + req.Model)}, nil
}

func (e *codexTierExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func newFallbackManager(t *testing.T, clientID string, executor *codexTierExecutor, fallback []internalconfig.CodexModelFallback, models ...string) *Manager {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	cfg := &internalconfig.Config{}
	cfg.Codex.ModelFallback = fallback
	manager.SetConfig(cfg)
	manager.RegisterExecutor(executor)

	infos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: model})
	}
	registry.GetGlobalRegistry().RegisterClient(clientID, "codex", infos)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(clientID) })
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: clientID, Provider: "codex"}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	return manager
}

func TestExecuteFallsBackOnOverload(t *testing.T) {
	executor := &codexTierExecutor{primary: "gpt-5.6-sol", cause: FailureCauseOverload}
	manager := newFallbackManager(t, "codex-fb-1", executor,
		[]internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra", "gpt-5.6-luna"}}},
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna")

	resp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-sol"}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	if string(resp.Payload) != "gpt-5.6-terra" {
		t.Fatalf("payload = %q, want gpt-5.6-terra", string(resp.Payload))
	}
}

func TestExecuteDoesNotFallBackOnQuota(t *testing.T) {
	executor := &codexTierExecutor{primary: "gpt-5.6-sol", cause: FailureCauseQuota}
	manager := newFallbackManager(t, "codex-fb-2", executor,
		[]internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}}},
		"gpt-5.6-sol", "gpt-5.6-terra")

	if _, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-sol"}, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("quota exhaustion must not fall back to another model")
	}
	for _, model := range executor.seen {
		if model != "gpt-5.6-sol" {
			t.Fatalf("executor ran %q, want only gpt-5.6-sol", model)
		}
	}
}

func TestExecuteWithoutConfigDoesNotFallBack(t *testing.T) {
	executor := &codexTierExecutor{primary: "gpt-5.6-sol", cause: FailureCauseOverload}
	manager := newFallbackManager(t, "codex-fb-3", executor, nil, "gpt-5.6-sol", "gpt-5.6-terra")

	if _, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-sol"}, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("empty configuration must preserve the original failure")
	}
	for _, model := range executor.seen {
		if model != "gpt-5.6-sol" {
			t.Fatalf("executor ran %q, want only gpt-5.6-sol", model)
		}
	}
}

func TestExecuteDoesNotFallBackForNativeCodexProtocol(t *testing.T) {
	executor := &codexTierExecutor{primary: "gpt-5.6-sol", cause: FailureCauseOverload}
	manager := newFallbackManager(t, "codex-fb-6", executor,
		[]internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}}},
		"gpt-5.6-sol", "gpt-5.6-terra")

	nativeOpts := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/responses"},
	}
	if _, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-sol"}, nativeOpts); errExecute == nil {
		t.Fatal("a native Codex Responses request must not be silently degraded to another model")
	}
	for _, model := range executor.seen {
		if model != "gpt-5.6-sol" {
			t.Fatalf("executor ran %q for a native Codex request, want only gpt-5.6-sol", model)
		}
	}
}

func TestExecuteCountFallsBackToCountNotCompletion(t *testing.T) {
	executor := &codexTierExecutor{primary: "gpt-5.6-sol", cause: FailureCauseOverload}
	manager := newFallbackManager(t, "codex-fb-7", executor,
		[]internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}}},
		"gpt-5.6-sol", "gpt-5.6-terra")

	resp, errExecute := manager.ExecuteCount(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-sol"}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute count: %v", errExecute)
	}
	if string(resp.Payload) != "count:gpt-5.6-terra" {
		t.Fatalf("payload = %q, want count:gpt-5.6-terra", string(resp.Payload))
	}
}

func TestExecuteStreamFallsBackOnOverload(t *testing.T) {
	executor := &codexTierExecutor{primary: "gpt-5.6-sol", cause: FailureCauseOverload}
	manager := newFallbackManager(t, "codex-fb-4", executor,
		[]internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}}},
		"gpt-5.6-sol", "gpt-5.6-terra")

	result, errStream := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-sol"}, cliproxyexecutor.Options{})
	if errStream != nil {
		t.Fatalf("execute stream: %v", errStream)
	}
	var payload []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if string(payload) != "gpt-5.6-terra" {
		t.Fatalf("payload = %q, want gpt-5.6-terra", string(payload))
	}
}

func TestFallbackWalksChainAndReturnsOriginalError(t *testing.T) {
	executor := &codexTierExecutor{primary: "gpt-5.6-sol", cause: FailureCauseOverload}
	manager := newFallbackManager(t, "codex-fb-5", executor,
		[]internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-absent"}}},
		"gpt-5.6-sol")

	_, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-sol"}, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("unusable chain must surface the original error")
	}
	var authErr *Error
	if !errors.As(errExecute, &authErr) || authErr == nil {
		t.Fatalf("error = %v, want *Error carrying the original overload failure", errExecute)
	}
	if authErr.HTTPStatus != http.StatusTooManyRequests || authErr.Message != "overloaded" {
		t.Fatalf("error = %+v, want the original 429 overload error, not a fallback-walk artifact (e.g. auth_not_found)", authErr)
	}
}

// TestShouldAttemptCodexModelFallbackCooldownOverload covers the
// modelCooldownError branch in shouldAttemptCodexModelFallback: it is the
// only path by which an all-credentials-cooling overload (a modelCooldownError
// carries no FailureCause()) reaches the fallback decision.
func TestShouldAttemptCodexModelFallbackCooldownOverload(t *testing.T) {
	executor := &codexTierExecutor{primary: "gpt-5.6-sol", cause: FailureCauseOverload}
	manager := newFallbackManager(t, "codex-fb-8", executor,
		[]internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}}},
		"gpt-5.6-sol", "gpt-5.6-terra")

	cooldownErr := newModelCooldownError("gpt-5.6-sol", "codex", time.Minute, FailureCauseOverload)
	if !manager.shouldAttemptCodexModelFallback(context.Background(), cooldownErr, []string{"codex"}, "gpt-5.6-sol", cliproxyexecutor.Options{}) {
		t.Fatal("an all-credentials-cooling overload must be eligible for fallback")
	}
}

// TestShouldAttemptCodexModelFallbackCooldownQuota is safety property 1 (never
// degrade on quota) reached through the cooldown path rather than a raw
// *Error with Cause set.
func TestShouldAttemptCodexModelFallbackCooldownQuota(t *testing.T) {
	executor := &codexTierExecutor{primary: "gpt-5.6-sol", cause: FailureCauseQuota}
	manager := newFallbackManager(t, "codex-fb-9", executor,
		[]internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}}},
		"gpt-5.6-sol", "gpt-5.6-terra")

	cooldownErr := newModelCooldownError("gpt-5.6-sol", "codex", time.Minute, FailureCauseQuota)
	if manager.shouldAttemptCodexModelFallback(context.Background(), cooldownErr, []string{"codex"}, "gpt-5.6-sol", cliproxyexecutor.Options{}) {
		t.Fatal("an all-credentials-cooling quota exhaustion must not be eligible for fallback")
	}
}

// TestShouldAttemptCodexModelFallbackDeclinesUnderHomeMode covers the Home
// gate: executeStreamMixedOnce ignores maxRetryCredentials when Home mode is
// enabled, so passing 1 there caps nothing and each fallback tier could burn
// through every Home credential during an overload incident. The manager
// must decline to degrade rather than run that unbounded walk.
func TestShouldAttemptCodexModelFallbackDeclinesUnderHomeMode(t *testing.T) {
	executor := &codexTierExecutor{primary: "gpt-5.6-sol", cause: FailureCauseOverload}
	manager := newFallbackManager(t, "codex-fb-10", executor,
		[]internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}}},
		"gpt-5.6-sol", "gpt-5.6-terra")

	homeCfg := &internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}}
	homeCfg.Codex.ModelFallback = []internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}}}
	manager.SetConfig(homeCfg)

	overloadErr := &Error{HTTPStatus: http.StatusTooManyRequests, Message: "overloaded", Cause: FailureCauseOverload}
	if manager.shouldAttemptCodexModelFallback(context.Background(), overloadErr, []string{"codex"}, "gpt-5.6-sol", cliproxyexecutor.Options{}) {
		t.Fatal("Home mode must not degrade: the one-credential-per-tier budget is void under Home dispatch")
	}
}
