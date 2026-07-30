package auth

import (
	"context"
	"net/http"
	"testing"

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

func (e *codexTierExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
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

	if _, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-sol"}, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("unusable chain must surface the original error")
	}
}
