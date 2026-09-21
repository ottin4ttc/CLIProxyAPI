package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

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

	mu       sync.Mutex
	chunks   chan cliproxyexecutor.StreamChunk
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
	e.mu.Lock()
	defer e.mu.Unlock()
	return &cliproxyexecutor.StreamResult{Chunks: e.chunks}, nil
}

func (e *inFlightGateExecutor) setChunks(chunks chan cliproxyexecutor.StreamChunk) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.chunks = chunks
}

func (e *inFlightGateExecutor) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.executed)
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

func TestCredentialInFlightLimiter(t *testing.T) {
	limiter := newCredentialInFlightLimiter()
	releaseA, _, okA := limiter.Acquire("a", 2)
	releaseB, _, okB := limiter.Acquire("a", 2)
	if !okA || !okB {
		t.Fatalf("first two acquires refused: %v %v", okA, okB)
	}
	_, wake, ok := limiter.Acquire("a", 2)
	if ok || wake == nil {
		t.Fatalf("third acquire over limit 2: admitted=%v wake=%v, want refused with a wake channel", ok, wake)
	}
	select {
	case <-wake:
		t.Fatalf("wake fired before any release")
	default:
	}
	if got := limiter.Active("a"); got != 2 {
		t.Fatalf("Active = %d, want 2", got)
	}
	releaseA()
	releaseA() // idempotent
	select {
	case <-wake:
	default:
		t.Fatalf("wake did not fire after release")
	}
	if got := limiter.Active("a"); got != 1 {
		t.Fatalf("Active after release = %d, want 1", got)
	}
	releaseB()
	if got := limiter.Active("a"); got != 0 {
		t.Fatalf("Active after both releases = %d, want 0", got)
	}
	if release, wake, ok := limiter.Acquire("a", 0); !ok || release != nil || wake != nil {
		t.Fatalf("limit 0 should admit without release or wake")
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

func TestExecuteRotatesOffBusyCredentialAndWaitsWhenAllBusy(t *testing.T) {
	ids := []string{"inflight-a", "inflight-b"}
	manager := newInFlightTestManager(t, ids, 1)
	executor := &inFlightGateExecutor{gate: make(chan struct{}), started: make(chan string, 4)}
	manager.RegisterExecutor(executor)
	req := cliproxyexecutor.Request{Model: "gpt"}

	results := make(chan error, 3)
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

	// Third request: every credential is busy, so it must queue at the proxy
	// and only reach the executor once a slot frees.
	go func() {
		_, errExecute := manager.Execute(context.Background(), []string{inFlightTestProvider}, req, cliproxyexecutor.Options{})
		results <- errExecute
	}()
	select {
	case id := <-executor.started:
		t.Fatalf("third request dispatched to %s while every credential was busy", id)
	case <-time.After(100 * time.Millisecond):
	}
	if calls := executor.calls(); calls != 2 {
		t.Fatalf("executor calls = %d, want 2 while waiting", calls)
	}

	close(executor.gate)
	if third := <-executor.started; third == "" {
		t.Fatalf("third request never dispatched after a slot freed")
	}
	for i := 0; i < 3; i++ {
		if errExecute := <-results; errExecute != nil {
			t.Fatalf("Execute() error = %v, want success", errExecute)
		}
	}
	for _, id := range ids {
		if got := manager.credentialInFlight.Active(id); got != 0 {
			t.Fatalf("Active(%s) = %d after completion, want 0", id, got)
		}
	}
}

func TestExecuteWaitingForSlotStopsWhenClientCancels(t *testing.T) {
	const id = "inflight-cancel"
	manager := newInFlightTestManager(t, []string{id}, 1)
	executor := &inFlightGateExecutor{gate: make(chan struct{}), started: make(chan string, 2)}
	manager.RegisterExecutor(executor)
	req := cliproxyexecutor.Request{Model: "gpt"}

	holder := make(chan error, 1)
	go func() {
		_, errExecute := manager.Execute(context.Background(), []string{inFlightTestProvider}, req, cliproxyexecutor.Options{})
		holder <- errExecute
	}()
	<-executor.started

	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() {
		_, errExecute := manager.Execute(ctx, []string{inFlightTestProvider}, req, cliproxyexecutor.Options{})
		waiter <- errExecute
	}()
	cancel()
	if errWait := <-waiter; !errors.Is(errWait, context.Canceled) {
		t.Fatalf("waiting request error = %v, want context.Canceled", errWait)
	}
	if calls := executor.calls(); calls != 1 {
		t.Fatalf("executor calls = %d, want 1 (cancelled waiter must not dispatch)", calls)
	}

	close(executor.gate)
	if errHolder := <-holder; errHolder != nil {
		t.Fatalf("holder Execute() error = %v", errHolder)
	}
	if got := manager.credentialInFlight.Active(id); got != 0 {
		t.Fatalf("Active = %d after completion, want 0", got)
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

	// The second stream must wait until the first one is drained.
	second := make(chan cliproxyexecutor.StreamChunk, 1)
	second <- cliproxyexecutor.StreamChunk{Payload: []byte("data: again\n\n")}
	close(second)
	executor.setChunks(second)
	waiter := make(chan error, 1)
	go func() {
		res, errAgain := manager.ExecuteStream(context.Background(), []string{inFlightTestProvider}, req, opts)
		if errAgain == nil {
			for range res.Chunks {
			}
		}
		waiter <- errAgain
	}()
	select {
	case errAgain := <-waiter:
		t.Fatalf("second stream returned %v while the first still held the slot", errAgain)
	case <-time.After(100 * time.Millisecond):
	}

	close(chunks)
	for range result.Chunks {
	}
	if errAgain := <-waiter; errAgain != nil {
		t.Fatalf("second ExecuteStream() error = %v, want success after drain", errAgain)
	}
	if got := manager.credentialInFlight.Active(id); got != 0 {
		t.Fatalf("Active = %d after both streams drained, want 0", got)
	}
}
