package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func overloadResult(authID, model string) Result {
	return Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    model,
		Success:  false,
		Error:    overloadError(),
	}
}

func overloadError() *Error {
	return &Error{
		Code:       "rate_limit",
		Message:    "Our servers are currently overloaded. Please try again later.",
		Retryable:  true,
		HTTPStatus: http.StatusTooManyRequests,
		Cause:      FailureCauseOverload,
	}
}

func registerOverloadAuth(t *testing.T, manager *Manager, auth *Auth) {
	t.Helper()
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}
}

func modelStateOrFatal(t *testing.T, manager *Manager, authID, model string) *ModelState {
	t.Helper()
	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil || updated.ModelStates[model] == nil {
		t.Fatalf("expected model state for %q on %q", model, authID)
	}
	return updated.ModelStates[model]
}

func expectModelAvailable(t *testing.T, manager *Manager, authID, model string) {
	t.Helper()
	updated, _ := manager.GetByID(authID)
	if blocked, reason, next := isAuthBlockedForModel(updated, model, time.Now()); blocked {
		t.Fatalf("expected %q to stay available after overload, blocked reason=%v next=%v", model, reason, next)
	}
}

// Upstream overload is a per-credential, memoryless signal: it feeds the
// health ring (see TestMarkResultRecordsOverloadInRing) and the weighted
// selector, never a cooldown window. A credential that was just shed must stay
// in rotation.
func TestMarkResultOverloadLeavesModelAvailable(t *testing.T) {
	withQuotaCooldownEnabled(t)

	hint := 42 * time.Second
	cases := []struct {
		name       string
		retryAfter *time.Duration
	}{
		{name: "no-hint", retryAfter: nil},
		{name: "retry-after-hint-ignored", retryAfter: &hint},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			auth := &Auth{ID: "auth-overload-" + tc.name, Provider: "codex", Metadata: map[string]any{"type": "codex"}}
			registerOverloadAuth(t, manager, auth)

			result := overloadResult(auth.ID, "gpt-5")
			result.RetryAfter = tc.retryAfter
			manager.MarkResult(context.Background(), result)

			state := modelStateOrFatal(t, manager, auth.ID, "gpt-5")
			if !state.Quota.NextRecoverAt.IsZero() || !state.NextRetryAfter.IsZero() {
				t.Fatalf("overload must not arm a window, got quota=%+v nextRetryAfter=%v", state.Quota, state.NextRetryAfter)
			}
			if state.Quota.Exceeded || state.Quota.BackoffLevel != 0 || state.Unavailable {
				t.Fatalf("overload must leave the model state clean, got %+v", state)
			}
			if state.LastError == nil || state.LastError.Cause != FailureCauseOverload {
				t.Fatalf("overload must still be recorded as the last error, got %+v", state.LastError)
			}
			expectModelAvailable(t, manager, auth.ID, "gpt-5")
			updated, _ := manager.GetByID(auth.ID)
			if !updated.NextRetryAfter.IsZero() || !updated.Quota.NextRecoverAt.IsZero() || updated.Unavailable {
				t.Fatalf("overload must not block the credential as a whole, got nextRetryAfter=%v quota=%+v unavailable=%v", updated.NextRetryAfter, updated.Quota, updated.Unavailable)
			}
		})
	}
}

func TestMarkResultOverloadKeepsLiveQuotaWindow(t *testing.T) {
	withQuotaCooldownEnabled(t)

	open := time.Now().Add(7 * time.Minute)
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-overload-live-quota",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: open,
				Quota:          QuotaState{Exceeded: true, Reason: FailureCauseQuota, NextRecoverAt: open, BackoffLevel: 2},
			},
		},
	}
	registerOverloadAuth(t, manager, auth)

	manager.MarkResult(context.Background(), overloadResult(auth.ID, "gpt-5"))
	state := modelStateOrFatal(t, manager, auth.ID, "gpt-5")
	if !state.Quota.NextRecoverAt.Equal(open) || state.Quota.BackoffLevel != 2 || state.Quota.Reason != FailureCauseQuota {
		t.Fatalf("a live quota window must survive an overload untouched, got %+v", state.Quota)
	}
	updated, _ := manager.GetByID(auth.ID)
	if blocked, _, _ := isAuthBlockedForModel(updated, "gpt-5", time.Now()); !blocked {
		t.Fatal("expected model to stay blocked by the live quota window")
	}
}

func TestMarkResultOverloadClearsExpiredQuotaWindow(t *testing.T) {
	withQuotaCooldownEnabled(t)

	expired := time.Now().Add(-time.Second)
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-overload-expired-quota",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: expired,
				Quota:          QuotaState{Exceeded: true, Reason: FailureCauseQuota, NextRecoverAt: expired, BackoffLevel: 6},
			},
		},
	}
	registerOverloadAuth(t, manager, auth)

	manager.MarkResult(context.Background(), overloadResult(auth.ID, "gpt-5"))
	state := modelStateOrFatal(t, manager, auth.ID, "gpt-5")
	if state.Quota.Exceeded || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 || state.Quota.Reason != "" {
		t.Fatalf("an expired quota window must be cleared, got %+v", state.Quota)
	}
	if state.Unavailable {
		t.Fatal("expected model to be available after the expired window is cleared")
	}
	expectModelAvailable(t, manager, auth.ID, "gpt-5")
}

func TestMarkResultOverloadKeepsLiveTransientCooldown(t *testing.T) {
	withQuotaCooldownEnabled(t)

	open := time.Now().Add(10 * time.Second)
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-overload-live-transient",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
		ModelStates: map[string]*ModelState{
			"gpt-5": {Status: StatusError, Unavailable: true, NextRetryAfter: open},
		},
	}
	registerOverloadAuth(t, manager, auth)

	manager.MarkResult(context.Background(), overloadResult(auth.ID, "gpt-5"))
	state := modelStateOrFatal(t, manager, auth.ID, "gpt-5")
	if !state.NextRetryAfter.Equal(open) || !state.Unavailable {
		t.Fatalf("a live transient cooldown must survive an overload untouched, got nextRetryAfter=%v unavailable=%v", state.NextRetryAfter, state.Unavailable)
	}
	updated, _ := manager.GetByID(auth.ID)
	if blocked, _, _ := isAuthBlockedForModel(updated, "gpt-5", time.Now()); !blocked {
		t.Fatal("expected model to stay blocked by the live transient cooldown")
	}
}

func TestApplyAuthFailureStateOverloadLeavesAuthAvailable(t *testing.T) {
	now := time.Now()
	auth := &Auth{ID: "auth-overload-credential", Provider: "codex"}

	applyAuthFailureState(auth, overloadError(), nil, now, false)

	if auth.Unavailable || !auth.NextRetryAfter.IsZero() || auth.Quota.Exceeded || !auth.Quota.NextRecoverAt.IsZero() || auth.Quota.BackoffLevel != 0 {
		t.Fatalf("credential-level overload must not block the credential, got unavailable=%v nextRetryAfter=%v quota=%+v", auth.Unavailable, auth.NextRetryAfter, auth.Quota)
	}
	if auth.LastError == nil || auth.LastError.Cause != FailureCauseOverload {
		t.Fatalf("overload must still be recorded as the last error, got %+v", auth.LastError)
	}
	if blocked, reason, _ := isAuthBlockedForModel(auth, "", now); blocked {
		t.Fatalf("expected credential to stay available, blocked reason=%v", reason)
	}
}

func TestMarkResultQuotaAfterOverloadReasonReseedsBackoffLevel(t *testing.T) {
	withQuotaCooldownEnabled(t)

	// A window carrying the overload reason can only come from persisted
	// cooldown state written before overload stopped arming windows; a quota
	// failure landing after it expires must start the quota ladder fresh.
	expired := time.Now().Add(-time.Second)
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-reseed-quota",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: expired,
				Quota:          QuotaState{Exceeded: true, Reason: FailureCauseOverload, NextRecoverAt: expired, BackoffLevel: 3},
			},
		},
	}
	registerOverloadAuth(t, manager, auth)

	manager.MarkResult(context.Background(), quotaResult(auth.ID, "gpt-5"))
	state := modelStateOrFatal(t, manager, auth.ID, "gpt-5")
	if state.Quota.BackoffLevel != 1 {
		t.Fatalf("BackoffLevel = %d, want reseeded 1", state.Quota.BackoffLevel)
	}
	d := time.Until(state.Quota.NextRecoverAt)
	if d <= 0 || d > 5*time.Second {
		t.Fatalf("quota window closes in %v, want about %v", d, quotaBackoffBase)
	}
	if state.Quota.Reason != FailureCauseQuota {
		t.Fatalf("Reason = %q, want %q", state.Quota.Reason, FailureCauseQuota)
	}
}

func TestMarkResultSuccessKeepsOpenCooldownWindow(t *testing.T) {
	withQuotaCooldownEnabled(t)

	// An overload window restored from persisted cooldown state is still
	// honoured until it expires; a success completing inside it comes from a
	// request admitted before the window was armed and must not clear it.
	open := time.Now().Add(7 * time.Minute)
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-overload-wipe",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: open,
				Quota:          QuotaState{Exceeded: true, Reason: FailureCauseOverload, NextRecoverAt: open, BackoffLevel: 2},
			},
		},
	}
	registerOverloadAuth(t, manager, auth)

	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5", Success: true})
	state := modelStateOrFatal(t, manager, auth.ID, "gpt-5")
	if !state.Quota.NextRecoverAt.Equal(open) {
		t.Fatalf("NextRecoverAt = %v, want untouched %v", state.Quota.NextRecoverAt, open)
	}
	if !state.Quota.Exceeded || state.Quota.BackoffLevel != 2 {
		t.Fatalf("quota state changed by in-window success: %+v", state.Quota)
	}
	updated, _ := manager.GetByID(auth.ID)
	if blocked, _, _ := isAuthBlockedForModel(updated, "gpt-5", time.Now()); !blocked {
		t.Fatal("expected model to stay blocked after in-window success")
	}
}

func TestMarkResultSuccessClearsExpiredCooldownWindow(t *testing.T) {
	withQuotaCooldownEnabled(t)

	expired := time.Now().Add(-time.Second)
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-overload-probe",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: expired,
				Quota:          QuotaState{Exceeded: true, Reason: FailureCauseOverload, NextRecoverAt: expired, BackoffLevel: 3},
			},
		},
	}
	registerOverloadAuth(t, manager, auth)

	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5", Success: true})
	state := modelStateOrFatal(t, manager, auth.ID, "gpt-5")
	if state.Unavailable || state.Quota.Exceeded || state.Quota.BackoffLevel != 0 {
		t.Fatalf("expected clean state after post-expiry success, got %+v", state)
	}
}

func TestUpdateAggregatedAvailabilitySharedOverloadReason(t *testing.T) {
	now := time.Now()
	future := now.Add(5 * time.Minute)
	auth := &Auth{
		ID: "auth-aggregate-overload",
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Unavailable:    true,
				NextRetryAfter: future,
				Quota:          QuotaState{Exceeded: true, Reason: FailureCauseOverload, NextRecoverAt: future, BackoffLevel: 1},
			},
			"gpt-6": {
				Unavailable:    true,
				NextRetryAfter: future,
				Quota:          QuotaState{Exceeded: true, Reason: FailureCauseOverload, NextRecoverAt: future, BackoffLevel: 2},
			},
		},
	}
	updateAggregatedAvailability(auth, now)
	if auth.Quota.Reason != FailureCauseOverload {
		t.Fatalf("aggregated Reason = %q, want %q", auth.Quota.Reason, FailureCauseOverload)
	}

	auth.ModelStates["gpt-6"].Quota.Reason = FailureCauseQuota
	updateAggregatedAvailability(auth, now)
	if auth.Quota.Reason != FailureCauseQuota {
		t.Fatalf("mixed aggregated Reason = %q, want %q", auth.Quota.Reason, FailureCauseQuota)
	}
}
