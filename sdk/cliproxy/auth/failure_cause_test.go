package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type causeCarryingError struct {
	cause string
}

func (e causeCarryingError) Error() string        { return "carrier" }
func (e causeCarryingError) StatusCode() int      { return http.StatusTooManyRequests }
func (e causeCarryingError) FailureCause() string { return e.cause }

func TestFailureCauseFromError(t *testing.T) {
	if got := failureCauseFromError(nil); got != "" {
		t.Fatalf("nil error cause = %q, want empty", got)
	}
	if got := failureCauseFromError(&Error{Message: "plain"}); got != "" {
		t.Fatalf("plain error cause = %q, want empty", got)
	}
	if got := failureCauseFromError(causeCarryingError{cause: FailureCauseOverload}); got != FailureCauseOverload {
		t.Fatalf("cause = %q, want %q", got, FailureCauseOverload)
	}
	wrapped := fmt.Errorf("wrapped: %w", causeCarryingError{cause: FailureCauseQuota})
	if got := failureCauseFromError(wrapped); got != FailureCauseQuota {
		t.Fatalf("wrapped cause = %q, want %q", got, FailureCauseQuota)
	}
}

func TestFailureCauseFromAuthError(t *testing.T) {
	if got := failureCauseFromError(&Error{Message: "x", Cause: FailureCauseOverload}); got != FailureCauseOverload {
		t.Fatalf("auth error cause = %q, want %q", got, FailureCauseOverload)
	}
}

func TestResultErrorFromErrorCarriesCause(t *testing.T) {
	resultErr := resultErrorFromError(causeCarryingError{cause: FailureCauseOverload})
	if resultErr == nil {
		t.Fatal("resultErrorFromError returned nil")
	}
	if resultErr.Cause != FailureCauseOverload {
		t.Fatalf("Cause = %q, want %q", resultErr.Cause, FailureCauseOverload)
	}
}

func TestCooldownReasonForModel(t *testing.T) {
	const model = "gpt-5.6-sol"
	overloaded := &Auth{ID: "a", ModelStates: map[string]*ModelState{
		model: {Unavailable: true, LastError: &Error{Cause: FailureCauseOverload}},
	}}
	quota := &Auth{ID: "b", ModelStates: map[string]*ModelState{
		model: {Unavailable: true, LastError: &Error{Cause: FailureCauseQuota}},
	}}

	if got := cooldownReasonForModel([]*Auth{overloaded}, model); got != FailureCauseOverload {
		t.Fatalf("all-overload reason = %q, want %q", got, FailureCauseOverload)
	}
	if got := cooldownReasonForModel([]*Auth{overloaded, quota}, model); got != "" {
		t.Fatalf("mixed reason = %q, want empty", got)
	}
	if got := cooldownReasonForModel([]*Auth{quota}, model); got != FailureCauseQuota {
		t.Fatalf("all-quota reason = %q, want %q", got, FailureCauseQuota)
	}
	if got := cooldownReasonForModel(nil, model); got != "" {
		t.Fatalf("empty reason = %q, want empty", got)
	}
}

func TestModelCooldownErrorCarriesReason(t *testing.T) {
	err := newModelCooldownError("gpt-5.6-sol", "codex", time.Minute, FailureCauseOverload)
	if err.reason != FailureCauseOverload {
		t.Fatalf("reason = %q, want %q", err.reason, FailureCauseOverload)
	}
	if err.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("StatusCode() = %d, want 429", err.StatusCode())
	}
	// reason is an internal routing signal only; it must never reach the wire.
	if strings.Contains(err.Error(), "reason") {
		t.Fatalf("Error() = %q, must not leak the internal reason field", err.Error())
	}
}

func (e causeCarryingError) RetryAfter() *time.Duration {
	d := time.Second
	return &d
}

func TestCodexNativeRequest(t *testing.T) {
	responsesOpts := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/responses"},
	}
	if !codexNativeRequest(responsesOpts) {
		t.Fatal("/v1/responses must count as a native Codex request")
	}

	originatorOpts := cliproxyexecutor.Options{
		Headers:  http.Header{"Originator": {"codex_cli_rs"}},
		Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/chat/completions"},
	}
	if !codexNativeRequest(originatorOpts) {
		t.Fatal("a client-sent Originator header must count as a native Codex request")
	}

	translatedOpts := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/chat/completions"},
	}
	if codexNativeRequest(translatedOpts) {
		t.Fatal("/v1/chat/completions without Originator must not count as native")
	}

	messagesOpts := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/messages"},
	}
	if codexNativeRequest(messagesOpts) {
		t.Fatal("/v1/messages without Originator must not count as native")
	}

	if codexNativeRequest(cliproxyexecutor.Options{}) {
		t.Fatal("empty options must not count as native")
	}
}

func TestCodexFallbackEligible(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	cfg := &internalconfig.Config{}
	cfg.Codex.ModelFallback = []internalconfig.CodexModelFallback{
		{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}},
	}
	manager.SetConfig(cfg)

	translated := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/chat/completions"},
	}
	native := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/responses"},
	}

	if !manager.codexFallbackEligible([]string{"codex"}, "gpt-5.6-sol", translated) {
		t.Fatal("translated codex request with a configured chain must be eligible")
	}
	if manager.codexFallbackEligible([]string{"codex"}, "gpt-5.6-sol", native) {
		t.Fatal("native Codex protocol request must never be eligible")
	}
	if manager.codexFallbackEligible([]string{"claude"}, "gpt-5.6-sol", translated) {
		t.Fatal("non-codex provider must not be eligible")
	}
	if manager.codexFallbackEligible([]string{"codex"}, "gpt-5.6-terra", translated) {
		t.Fatal("model without a configured chain must not be eligible")
	}

	empty := NewManager(nil, nil, nil)
	empty.SetConfig(&internalconfig.Config{})
	if empty.codexFallbackEligible([]string{"codex"}, "gpt-5.6-sol", translated) {
		t.Fatal("empty configuration must not be eligible")
	}
}

func TestShouldRetryAfterErrorStopsRotatingOnOverload(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(3, 30*time.Second, 0)
	// shouldRetryAfterError's status-429 branch gates on retryAllowed, which
	// only permits a retry when a matching provider auth is registered (see
	// TestManager_ShouldRetryAfterError_SkipsWrappedHomeConcurrencyBusy for the
	// same pattern). Register one so the assertions below exercise the new
	// fallback-cap logic instead of failing on that unrelated precondition.
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "overload-auth", Provider: "codex"}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	overloadErr := causeCarryingError{cause: FailureCauseOverload}
	if _, retry := manager.shouldRetryAfterError(overloadErr, 0, []string{"codex"}, "gpt-5.6-sol", 30*time.Second, true); !retry {
		t.Fatal("attempt 0 on overload should still rotate once")
	}
	if _, retry := manager.shouldRetryAfterError(overloadErr, 1, []string{"codex"}, "gpt-5.6-sol", 30*time.Second, true); retry {
		t.Fatal("attempt 1 on overload must not rotate again when the request can fall back")
	}
	if _, retry := manager.shouldRetryAfterError(overloadErr, 1, []string{"codex"}, "gpt-5.6-sol", 30*time.Second, false); !retry {
		t.Fatal("a request that cannot fall back must keep rotating, exactly as before this change")
	}

	quotaErr := causeCarryingError{cause: FailureCauseQuota}
	if _, retry := manager.shouldRetryAfterError(quotaErr, 1, []string{"codex"}, "gpt-5.6-sol", 30*time.Second, true); !retry {
		t.Fatal("quota errors must keep rotating at attempt 1")
	}
}
