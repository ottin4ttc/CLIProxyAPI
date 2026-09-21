package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const inFlightTestProvider = "codex"

// inFlightGateExecutor blocks every Execute until gate is closed and reports
// the auth ID of each call on started. ExecuteStream hands out the chunks
// channel the test controls, so a stream stays open until the test closes it.
type inFlightGateExecutor struct {
	gate    chan struct{}
	started chan string
	chunks  chan cliproxyexecutor.StreamChunk

	mu       sync.Mutex
	executed []string
}

func (*inFlightGateExecutor) Identifier() string { return inFlightTestProvider }

func (e *inFlightGateExecutor) Execute(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executed = append(e.executed, auth.ID)
	e.mu.Unlock()
	e.started <- auth.ID
	select {
	case <-e.gate:
	case <-ctx.Done():
		return cliproxyexecutor.Response{}, ctx.Err()
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *inFlightGateExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return &cliproxyexecutor.StreamResult{Chunks: e.chunks}, nil
}

func (*inFlightGateExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (*inFlightGateExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (*inFlightGateExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func newInFlightTestManager(t *testing.T, authIDs []string, limit int) *Manager {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{CredentialMaxInFlight: map[string]int{inFlightTestProvider: limit}})
	manager.SetRetryConfig(0, 0, 0)
	reg := registry.GetGlobalRegistry()
	for _, id := range authIDs {
		reg.RegisterClient(id, inFlightTestProvider, []*registry.ModelInfo{{ID: "gpt"}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, errRegister := manager.Register(context.Background(), &Auth{
			ID:       id,
			Provider: inFlightTestProvider,
			Metadata: map[string]any{"disable_cooling": true},
		}); errRegister != nil {
			t.Fatalf("register %s: %v", id, errRegister)
		}
	}
	return manager
}

func assertInFlightExceeded(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected in-flight exceeded error, got nil")
	}
	var busy *HomeConcurrencyBusyError
	if !errors.As(err, &busy) || busy == nil {
		t.Fatalf("error = %v, want HomeConcurrencyBusyError", err)
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		t.Fatalf("error = %v, want *Error", err)
	}
	if authErr.Code != "credential_inflight_exceeded" || authErr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("error code/status = %s/%d, want credential_inflight_exceeded/429", authErr.Code, authErr.HTTPStatus)
	}
}

func TestCredentialInFlightLimiter(t *testing.T) {
	limiter := newCredentialInFlightLimiter()
	releaseA, okA := limiter.Acquire("a", 2)
	releaseB, okB := limiter.Acquire("a", 2)
	if !okA || !okB {
		t.Fatalf("first two acquires refused: %v %v", okA, okB)
	}
	if _, ok := limiter.Acquire("a", 2); ok {
		t.Fatalf("third acquire admitted over limit 2")
	}
	if got := limiter.Active("a"); got != 2 {
		t.Fatalf("Active = %d, want 2", got)
	}
	releaseA()
	releaseA() // idempotent
	if got := limiter.Active("a"); got != 1 {
		t.Fatalf("Active after release = %d, want 1", got)
	}
	releaseB()
	if got := limiter.Active("a"); got != 0 {
		t.Fatalf("Active after both releases = %d, want 0", got)
	}
	if release, ok := limiter.Acquire("a", 0); !ok || release != nil {
		t.Fatalf("limit 0 should admit without a release func")
	}
}

func TestCredentialInFlightLimitProviderScope(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{CredentialMaxInFlight: map[string]int{"Codex": 1, "gemini": 0}})
	if got := manager.credentialInFlightLimit("codex"); got != 1 {
		t.Fatalf("codex limit = %d, want 1 (case-insensitive)", got)
	}
	if got := manager.credentialInFlightLimit("gemini"); got != 0 {
		t.Fatalf("gemini limit = %d, want 0", got)
	}
	if got := manager.credentialInFlightLimit("claude"); got != 0 {
		t.Fatalf("unlisted provider limit = %d, want 0", got)
	}
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}, CredentialMaxInFlight: map[string]int{"codex": 1}})
	if got := manager.credentialInFlightLimit("codex"); got != 0 {
		t.Fatalf("Home mode limit = %d, want 0", got)
	}
}

func TestExecuteRotatesOffBusyCredentialAndRejectsWhenAllBusy(t *testing.T) {
	ids := []string{"inflight-a", "inflight-b"}
	manager := newInFlightTestManager(t, ids, 1)
	executor := &inFlightGateExecutor{gate: make(chan struct{}), started: make(chan string, 4)}
	manager.RegisterExecutor(executor)
	req := cliproxyexecutor.Request{Model: "gpt"}

	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, errExecute := manager.Execute(context.Background(), []string{inFlightTestProvider}, req, cliproxyexecutor.Options{})
			results <- errExecute
		}()
	}
	first, second := <-executor.started, <-executor.started
	if first == second {
		t.Fatalf("both concurrent requests ran on %s, want rotation to the other credential", first)
	}
	for _, id := range ids {
		if got := manager.credentialInFlight.Active(id); got != 1 {
			t.Fatalf("Active(%s) = %d, want 1 while executing", id, got)
		}
	}

	_, errThird := manager.Execute(context.Background(), []string{inFlightTestProvider}, req, cliproxyexecutor.Options{})
	assertInFlightExceeded(t, errThird)
	if calls := len(executor.executed); calls != 2 {
		t.Fatalf("executor calls = %d, want 2 (rejected request must not reach the executor)", calls)
	}

	close(executor.gate)
	for i := 0; i < 2; i++ {
		if errExecute := <-results; errExecute != nil {
			t.Fatalf("Execute() error = %v, want success", errExecute)
		}
	}
	for _, id := range ids {
		if got := manager.credentialInFlight.Active(id); got != 0 {
			t.Fatalf("Active(%s) = %d after completion, want 0", id, got)
		}
	}
	_, errFourth := manager.Execute(context.Background(), []string{inFlightTestProvider}, req, cliproxyexecutor.Options{})
	if errFourth != nil {
		t.Fatalf("Execute() after release error = %v, want success", errFourth)
	}
}

func TestExecuteStreamHoldsSlotUntilDrained(t *testing.T) {
	const id = "inflight-stream"
	manager := newInFlightTestManager(t, []string{id}, 1)
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: first\n\n")}
	executor := &inFlightGateExecutor{gate: make(chan struct{}), started: make(chan string, 1), chunks: chunks}
	manager.RegisterExecutor(executor)
	req := cliproxyexecutor.Request{Model: "gpt"}
	opts := cliproxyexecutor.Options{Stream: true}

	result, errStream := manager.ExecuteStream(context.Background(), []string{inFlightTestProvider}, req, opts)
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	if got := manager.credentialInFlight.Active(id); got != 1 {
		t.Fatalf("Active = %d while stream open, want 1", got)
	}
	_, errBusy := manager.ExecuteStream(context.Background(), []string{inFlightTestProvider}, req, opts)
	assertInFlightExceeded(t, errBusy)

	close(chunks)
	for range result.Chunks {
	}
	if got := manager.credentialInFlight.Active(id); got != 0 {
		t.Fatalf("Active = %d after stream drained, want 0", got)
	}

	executor.chunks = make(chan cliproxyexecutor.StreamChunk, 1)
	executor.chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: again\n\n")}
	close(executor.chunks)
	if _, errAgain := manager.ExecuteStream(context.Background(), []string{inFlightTestProvider}, req, opts); errAgain != nil {
		t.Fatalf("ExecuteStream() after drain error = %v, want success", errAgain)
	}
}
