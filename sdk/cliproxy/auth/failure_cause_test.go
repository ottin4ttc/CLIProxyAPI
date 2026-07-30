package auth

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
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
