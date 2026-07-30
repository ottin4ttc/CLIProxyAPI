package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// codexTierExecutor fails the models listed in failing and succeeds on any
// other model. Responses are JSON labeled with the model that actually ran,
// mirroring the real codex executor, which stamps sdktranslator's model
// argument - req.Model, never the requested-model metadata - into the
// translated response. Tests can therefore assert what a client would see
// rather than what the executor merely observed.
type codexTierExecutor struct {
	primary string
	cause   string
	// extraFailing lists further models that must also fail, so a test can
	// drive the chain past its first tier.
	extraFailing []string
	seen         []string
	// seenRequestedModel records opts.Metadata[RequestedModelMetadataKey] for
	// every call, in the same order as seen.
	seenRequestedModel []string
	// seenAuth records the auth ID each call ran on, in the same order as seen.
	seenAuth []string
}

func requestedModelFromOpts(opts cliproxyexecutor.Options) string {
	value, _ := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey].(string)
	return value
}

// codexTierPayload is the JSON body the fake executor returns, labeled with the
// model that ran. kind separates completion bodies from count bodies.
func codexTierPayload(kind, model string) []byte {
	return []byte(`{"object":"` + kind + `","model":"` + model + `"}`)
}

// responseModel reports the model a response body claims to have been served
// by, which is what the client ultimately sees.
func responseModel(t *testing.T, payload []byte) string {
	t.Helper()
	var decoded struct {
		Object string `json:"object"`
		Model  string `json:"model"`
	}
	if errDecode := json.Unmarshal(payload, &decoded); errDecode != nil {
		t.Fatalf("decode response payload %q: %v", string(payload), errDecode)
	}
	return decoded.Model
}

func responseObject(t *testing.T, payload []byte) string {
	t.Helper()
	var decoded struct {
		Object string `json:"object"`
	}
	if errDecode := json.Unmarshal(payload, &decoded); errDecode != nil {
		t.Fatalf("decode response payload %q: %v", string(payload), errDecode)
	}
	return decoded.Object
}

func (e *codexTierExecutor) Identifier() string { return "codex" }

func (e *codexTierExecutor) fails(model string) bool {
	if model == e.primary {
		return true
	}
	for _, failing := range e.extraFailing {
		if model == failing {
			return true
		}
	}
	return false
}

func (e *codexTierExecutor) record(auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) {
	e.seen = append(e.seen, req.Model)
	e.seenRequestedModel = append(e.seenRequestedModel, requestedModelFromOpts(opts))
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.seenAuth = append(e.seenAuth, authID)
}

func (e *codexTierExecutor) Execute(_ context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.record(auth, req, opts)
	if e.fails(req.Model) {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "overloaded", Cause: e.cause}
	}
	return cliproxyexecutor.Response{Payload: codexTierPayload("response", req.Model)}, nil
}

func (e *codexTierExecutor) ExecuteStream(_ context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.record(auth, req, opts)
	if e.fails(req.Model) {
		return nil, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "overloaded", Cause: e.cause}
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: codexTierPayload("response.chunk", req.Model)}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *codexTierExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (e *codexTierExecutor) CountTokens(_ context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.record(auth, req, opts)
	if e.fails(req.Model) {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "overloaded", Cause: e.cause}
	}
	return cliproxyexecutor.Response{Payload: codexTierPayload("count", req.Model)}, nil
}

func (e *codexTierExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func newFallbackManager(t *testing.T, clientID string, executor *codexTierExecutor, fallback []internalconfig.CodexModelFallback, models ...string) *Manager {
	t.Helper()
	return newFallbackManagerWithAuths(t, clientID, 1, executor, fallback, models...)
}

// newFallbackManagerWithAuths registers authCount codex credentials, all
// serving the same models, so a test can distinguish "one credential per tier"
// from "every credential per tier".
func newFallbackManagerWithAuths(t *testing.T, clientID string, authCount int, executor *codexTierExecutor, fallback []internalconfig.CodexModelFallback, models ...string) *Manager {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	cfg := &internalconfig.Config{}
	cfg.Codex.ModelFallback = fallback
	manager.SetConfig(cfg)
	manager.RegisterExecutor(executor)

	for i := 0; i < authCount; i++ {
		authID := clientID
		if i > 0 {
			authID = clientID + "-" + strconv.Itoa(i)
		}
		infos := make([]*registry.ModelInfo, 0, len(models))
		for _, model := range models {
			infos = append(infos, &registry.ModelInfo{ID: model})
		}
		registry.GetGlobalRegistry().RegisterClient(authID, "codex", infos)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: "codex"}); errRegister != nil {
			t.Fatalf("register auth %s: %v", authID, errRegister)
		}
	}
	return manager
}

// countModelAttempts reports how many times the executor was asked to run model.
func countModelAttempts(seen []string, model string) int {
	count := 0
	for _, ran := range seen {
		if ran == model {
			count++
		}
	}
	return count
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
	if got := responseModel(t, resp.Payload); got != "gpt-5.6-terra" {
		t.Fatalf("response model = %q, want gpt-5.6-terra", got)
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
	if got := responseObject(t, resp.Payload); got != "count" {
		t.Fatalf("response object = %q, want count (the count executor, not the completion executor)", got)
	}
	if got := responseModel(t, resp.Payload); got != "gpt-5.6-terra" {
		t.Fatalf("response model = %q, want gpt-5.6-terra", got)
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
	if got := responseModel(t, payload); got != "gpt-5.6-terra" {
		t.Fatalf("stream response model = %q, want gpt-5.6-terra", got)
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

// TestCodexFallbackEligibleDeclinesUnderHomeMode pins the Home gate to
// codexFallbackEligible rather than to the fallback decision alone.
// codexFallbackEligible gates both halves of the feature: the rotation cap in
// shouldRetryAfterError and the degrade itself. ExecuteStream is the one entry
// point whose retry loop runs under Home mode, so a Home gate that lived only
// in shouldAttemptCodexModelFallback would cap rotation at attempt >= 1 and
// then refuse to degrade - a 429 after two sweeps where Home mode previously
// kept rotating, and no degraded response either.
func TestCodexFallbackEligibleDeclinesUnderHomeMode(t *testing.T) {
	executor := &codexTierExecutor{primary: "gpt-5.6-sol", cause: FailureCauseOverload}
	manager := newFallbackManager(t, "codex-fb-14", executor,
		[]internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}}},
		"gpt-5.6-sol", "gpt-5.6-terra")

	if !manager.codexFallbackEligible([]string{"codex"}, "gpt-5.6-sol", cliproxyexecutor.Options{}) {
		t.Fatal("a configured chain on a translated codex request must be eligible without Home mode")
	}

	homeCfg := &internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}}
	homeCfg.Codex.ModelFallback = []internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}}}
	manager.SetConfig(homeCfg)

	if manager.codexFallbackEligible([]string{"codex"}, "gpt-5.6-sol", cliproxyexecutor.Options{}) {
		t.Fatal("Home mode must make the request ineligible, so the rotation cap in shouldRetryAfterError never fires either")
	}
}

// TestShouldAttemptCodexModelFallbackDeclinesUnderHomeMode covers the other
// half of the same gate: the degrade itself must stay declined under Home mode.
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

// TestExecuteFallbackResponseReportsTheServingTier covers design requirement 6
// at the level that matters: the body the client receives. The auth layer's
// only response relabeling (rewriteForceMappedResponse and the wrapStreamResult
// rewriter) is gated on OAuthModelAliasResult.ForceMapping, so on the normal
// path nothing rewrites the model the executor stamped from req.Model - which
// the walker set to the tier. The requested-model metadata must therefore stay
// at the model the client asked for, which is what keeps requested_model !=
// resolved_model usable as the fallback-rate query in usage_events.
func TestExecuteFallbackResponseReportsTheServingTier(t *testing.T) {
	executor := &codexTierExecutor{primary: "gpt-5.6-sol", cause: FailureCauseOverload}
	manager := newFallbackManager(t, "codex-fb-11", executor,
		[]internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}}},
		"gpt-5.6-sol", "gpt-5.6-terra")

	callerMeta := map[string]any{cliproxyexecutor.RequestedModelMetadataKey: "gpt-5.6-sol"}
	opts := cliproxyexecutor.Options{Metadata: callerMeta}

	resp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-sol"}, opts)
	if errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}

	if got := responseModel(t, resp.Payload); got != "gpt-5.6-terra" {
		t.Fatalf("response model = %q, want gpt-5.6-terra (the tier that served it)", got)
	}
	if len(executor.seenRequestedModel) == 0 {
		t.Fatal("executor observed no calls")
	}
	if last := executor.seenRequestedModel[len(executor.seenRequestedModel)-1]; last != "gpt-5.6-sol" {
		t.Fatalf("requested-model metadata seen by the serving tier = %q, want the client's gpt-5.6-sol so usage accounting can still measure the fallback rate", last)
	}
	if got := callerMeta[cliproxyexecutor.RequestedModelMetadataKey]; got != "gpt-5.6-sol" {
		t.Fatalf("caller's requested-model metadata = %v, want unchanged gpt-5.6-sol", got)
	}
}

// TestExecuteStreamFallbackResponseReportsTheServingTier is the streaming
// counterpart: it also covers the wrapStreamResult rewriter, which would
// rewrite the model field of this JSON chunk if it were ever engaged.
func TestExecuteStreamFallbackResponseReportsTheServingTier(t *testing.T) {
	executor := &codexTierExecutor{primary: "gpt-5.6-sol", cause: FailureCauseOverload}
	manager := newFallbackManager(t, "codex-fb-12", executor,
		[]internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}}},
		"gpt-5.6-sol", "gpt-5.6-terra")

	callerMeta := map[string]any{cliproxyexecutor.RequestedModelMetadataKey: "gpt-5.6-sol"}
	opts := cliproxyexecutor.Options{Metadata: callerMeta}

	result, errStream := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-sol"}, opts)
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

	if got := responseModel(t, payload); got != "gpt-5.6-terra" {
		t.Fatalf("stream response model = %q, want gpt-5.6-terra (the tier that served it)", got)
	}
	if len(executor.seenRequestedModel) == 0 {
		t.Fatal("executor observed no calls")
	}
	if last := executor.seenRequestedModel[len(executor.seenRequestedModel)-1]; last != "gpt-5.6-sol" {
		t.Fatalf("requested-model metadata seen by the serving tier = %q, want the client's gpt-5.6-sol", last)
	}
	if got := callerMeta[cliproxyexecutor.RequestedModelMetadataKey]; got != "gpt-5.6-sol" {
		t.Fatalf("caller's requested-model metadata = %v, want unchanged gpt-5.6-sol", got)
	}
}

// TestExecuteCountFallbackResponseReportsTheServingTier is the count-tokens
// counterpart.
func TestExecuteCountFallbackResponseReportsTheServingTier(t *testing.T) {
	executor := &codexTierExecutor{primary: "gpt-5.6-sol", cause: FailureCauseOverload}
	manager := newFallbackManager(t, "codex-fb-13", executor,
		[]internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}}},
		"gpt-5.6-sol", "gpt-5.6-terra")

	callerMeta := map[string]any{cliproxyexecutor.RequestedModelMetadataKey: "gpt-5.6-sol"}
	opts := cliproxyexecutor.Options{Metadata: callerMeta}

	resp, errExecute := manager.ExecuteCount(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-sol"}, opts)
	if errExecute != nil {
		t.Fatalf("execute count: %v", errExecute)
	}

	if got := responseModel(t, resp.Payload); got != "gpt-5.6-terra" {
		t.Fatalf("count response model = %q, want gpt-5.6-terra (the tier that served it)", got)
	}
	if got := callerMeta[cliproxyexecutor.RequestedModelMetadataKey]; got != "gpt-5.6-sol" {
		t.Fatalf("caller's requested-model metadata = %v, want unchanged gpt-5.6-sol", got)
	}
}

// TestFallbackTriesExactlyOneCredentialPerTier covers safety property 5's first
// half. Two codex credentials are registered, and the fallback tier fails on
// both, so an unbounded walk would attempt the tier twice. The walker passes 1
// as maxRetryCredentials precisely to prevent that: degrading is already the
// emergency path, and exhausting the pool on every tier multiplies the cost of
// an overload incident by the chain length.
func TestFallbackTriesExactlyOneCredentialPerTier(t *testing.T) {
	executor := &codexTierExecutor{
		primary:      "gpt-5.6-sol",
		cause:        FailureCauseOverload,
		extraFailing: []string{"gpt-5.6-terra"},
	}
	manager := newFallbackManagerWithAuths(t, "codex-fb-15", 2, executor,
		[]internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}}},
		"gpt-5.6-sol", "gpt-5.6-terra")

	if _, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-sol"}, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("both the primary model and the only fallback tier failed, so the request must fail")
	}

	// The primary sweep proves both credentials were usable, so the tier's
	// single attempt is a real cap rather than an artifact of a one-auth pool.
	if got := countModelAttempts(executor.seen, "gpt-5.6-sol"); got != 2 {
		t.Fatalf("primary model attempts = %d, want 2 (one full sweep of both credentials); seen=%v", got, executor.seen)
	}
	if got := countModelAttempts(executor.seen, "gpt-5.6-terra"); got != 1 {
		t.Fatalf("fallback tier attempts = %d, want exactly 1 credential per tier; seen=%v auths=%v", got, executor.seen, executor.seenAuth)
	}
}

// TestFallbackWalksToTheNextTierWhenTheFirstFails covers safety property 5's
// second half: the chain is walked once, in order. The first tier fails and the
// second succeeds, which is the only shape that exercises the continue-to-next-
// tier branch with a following success.
func TestFallbackWalksToTheNextTierWhenTheFirstFails(t *testing.T) {
	executor := &codexTierExecutor{
		primary:      "gpt-5.6-sol",
		cause:        FailureCauseOverload,
		extraFailing: []string{"gpt-5.6-terra"},
	}
	manager := newFallbackManager(t, "codex-fb-16", executor,
		[]internalconfig.CodexModelFallback{{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra", "gpt-5.6-luna"}}},
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna")

	resp, errExecute := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-sol"}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	if got := responseModel(t, resp.Payload); got != "gpt-5.6-luna" {
		t.Fatalf("response model = %q, want gpt-5.6-luna (the second tier, after the first also failed)", got)
	}

	terraAt, lunaAt := -1, -1
	for i, ran := range executor.seen {
		if ran == "gpt-5.6-terra" && terraAt < 0 {
			terraAt = i
		}
		if ran == "gpt-5.6-luna" && lunaAt < 0 {
			lunaAt = i
		}
	}
	if terraAt < 0 || lunaAt < 0 {
		t.Fatalf("both tiers must be attempted; seen=%v", executor.seen)
	}
	if terraAt > lunaAt {
		t.Fatalf("chain walked out of order: terra at %d, luna at %d; seen=%v", terraAt, lunaAt, executor.seen)
	}
	if got := countModelAttempts(executor.seen, "gpt-5.6-luna"); got != 1 {
		t.Fatalf("second tier attempts = %d, want 1 (the chain is walked once); seen=%v", got, executor.seen)
	}
}
