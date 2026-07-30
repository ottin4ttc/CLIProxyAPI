package auth

import (
	"fmt"
	"net/http"
	"testing"
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
