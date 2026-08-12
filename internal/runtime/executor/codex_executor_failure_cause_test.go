package executor

import (
	"net/http"
	"testing"
	"time"
)

func TestNewCodexStatusErrFailureCause(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name:   "server_is_overloaded",
			status: http.StatusOK,
			body:   `{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded."}}`,
			want:   codexFailureCauseOverload,
		},
		{
			name:   "slow_down",
			status: http.StatusOK,
			body:   `{"error":{"code":"slow_down","message":"slow down"}}`,
			want:   codexFailureCauseOverload,
		},
		{
			name:   "model at capacity",
			status: http.StatusOK,
			body:   `{"error":{"message":"Selected model is at capacity. Please try a different model."}}`,
			want:   codexFailureCauseOverload,
		},
		{
			name:   "usage limit reached",
			status: http.StatusOK,
			body:   `{"error":{"type":"usage_limit_reached","resets_in_seconds":60}}`,
			want:   codexFailureCauseQuota,
		},
		{
			name:   "unrelated server error",
			status: http.StatusBadGateway,
			body:   `{"error":{"type":"server_error","code":"server_error","message":"boom"}}`,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newCodexStatusErr(tt.status, []byte(tt.body))
			if got := err.FailureCause(); got != tt.want {
				t.Fatalf("FailureCause() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStatusErrFailureCauseDefaultsEmpty(t *testing.T) {
	err := statusErr{code: http.StatusTooManyRequests, msg: "plain"}
	if got := err.FailureCause(); got != "" {
		t.Fatalf("FailureCause() = %q, want empty", got)
	}
}

// Overload/capacity 429s must not carry a retryAfter hint: the auth layer
// routes cause "overload" onto its own escalating cooldown ladder.
func TestNewCodexStatusErrOverloadCarriesNoRetryAfter(t *testing.T) {
	bodies := []string{
		`{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded."}}`,
		`{"error":{"message":"Selected model is at capacity. Please try a different model."}}`,
	}
	for _, body := range bodies {
		err := newCodexStatusErr(http.StatusOK, []byte(body))
		if got := err.StatusCode(); got != http.StatusTooManyRequests {
			t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
		}
		if err.RetryAfter() != nil {
			t.Fatalf("retryAfter = %v, want nil for overload/capacity", *err.RetryAfter())
		}
	}
}

// usage_limit_reached keeps its parsed reset metadata.
func TestNewCodexStatusErrUsageLimitKeepsRetryAfter(t *testing.T) {
	err := newCodexStatusErr(http.StatusOK, []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":60}}`))
	if err.RetryAfter() == nil || *err.RetryAfter() != 60*time.Second {
		t.Fatalf("retryAfter = %v, want 60s from resets_in_seconds", err.RetryAfter())
	}
}
