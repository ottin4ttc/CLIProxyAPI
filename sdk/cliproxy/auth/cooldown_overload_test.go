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
		Error: &Error{
			Code:       "rate_limit",
			Message:    "Our servers are currently overloaded. Please try again later.",
			Retryable:  true,
			HTTPStatus: http.StatusTooManyRequests,
			Cause:      FailureCauseOverload,
		},
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

func expectWindowAround(t *testing.T, at time.Time, want time.Duration) {
	t.Helper()
	d := time.Until(at)
	if d < want-30*time.Second || d > want+30*time.Second {
		t.Fatalf("window closes in %v, want about %v", d, want)
	}
}

func TestMarkResultOverloadTakesFiveMinuteLadder(t *testing.T) {
	withQuotaCooldownEnabled(t)

	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-overload-base", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	registerOverloadAuth(t, manager, auth)

	manager.MarkResult(context.Background(), overloadResult(auth.ID, "gpt-5"))
	state := modelStateOrFatal(t, manager, auth.ID, "gpt-5")
	if state.Quota.Reason != FailureCauseOverload {
		t.Fatalf("Reason = %q, want %q", state.Quota.Reason, FailureCauseOverload)
	}
	if state.Quota.BackoffLevel != 1 {
		t.Fatalf("BackoffLevel = %d, want 1", state.Quota.BackoffLevel)
	}
	expectWindowAround(t, state.Quota.NextRecoverAt, overloadBackoffBase)
	if !state.NextRetryAfter.Equal(state.Quota.NextRecoverAt) {
		t.Fatalf("NextRetryAfter %v != NextRecoverAt %v", state.NextRetryAfter, state.Quota.NextRecoverAt)
	}
	updated, _ := manager.GetByID(auth.ID)
	if blocked, _, _ := isAuthBlockedForModel(updated, "gpt-5", time.Now()); !blocked {
		t.Fatal("expected model to be blocked during overload cooldown")
	}
}

func TestMarkResultOverloadWithRetryAfterStillTakesLadder(t *testing.T) {
	withQuotaCooldownEnabled(t)

	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-overload-hint", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	registerOverloadAuth(t, manager, auth)

	result := overloadResult(auth.ID, "gpt-5")
	hint := 42 * time.Second
	result.RetryAfter = &hint
	manager.MarkResult(context.Background(), result)

	state := modelStateOrFatal(t, manager, auth.ID, "gpt-5")
	expectWindowAround(t, state.Quota.NextRecoverAt, overloadBackoffBase)
	if state.Quota.BackoffLevel != 1 {
		t.Fatalf("BackoffLevel = %d, want 1", state.Quota.BackoffLevel)
	}
}

func TestMarkResultOverloadLadderEscalatesAfterExpiry(t *testing.T) {
	withQuotaCooldownEnabled(t)

	cases := []struct {
		name       string
		seedLevel  int
		wantLevel  int
		wantWindow time.Duration
	}{
		{name: "level1-to-10m", seedLevel: 1, wantLevel: 2, wantWindow: 10 * time.Minute},
		{name: "level2-to-20m", seedLevel: 2, wantLevel: 3, wantWindow: 20 * time.Minute},
		{name: "level3-caps-30m", seedLevel: 3, wantLevel: 3, wantWindow: 30 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expired := time.Now().Add(-time.Second)
			manager := NewManager(nil, nil, nil)
			auth := &Auth{
				ID:       "auth-overload-" + tc.name,
				Provider: "codex",
				Metadata: map[string]any{"type": "codex"},
				ModelStates: map[string]*ModelState{
					"gpt-5": {
						Status:         StatusError,
						Unavailable:    true,
						NextRetryAfter: expired,
						Quota:          QuotaState{Exceeded: true, Reason: FailureCauseOverload, NextRecoverAt: expired, BackoffLevel: tc.seedLevel},
					},
				},
			}
			registerOverloadAuth(t, manager, auth)

			manager.MarkResult(context.Background(), overloadResult(auth.ID, "gpt-5"))
			state := modelStateOrFatal(t, manager, auth.ID, "gpt-5")
			if state.Quota.BackoffLevel != tc.wantLevel {
				t.Fatalf("BackoffLevel = %d, want %d", state.Quota.BackoffLevel, tc.wantLevel)
			}
			expectWindowAround(t, state.Quota.NextRecoverAt, tc.wantWindow)
		})
	}
}

func TestMarkResultOverloadReusesOpenWindow(t *testing.T) {
	withQuotaCooldownEnabled(t)

	open := time.Now().Add(7 * time.Minute)
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-overload-reuse",
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

	manager.MarkResult(context.Background(), overloadResult(auth.ID, "gpt-5"))
	state := modelStateOrFatal(t, manager, auth.ID, "gpt-5")
	if state.Quota.BackoffLevel != 2 {
		t.Fatalf("BackoffLevel = %d, want 2 (in-window failure must not escalate)", state.Quota.BackoffLevel)
	}
	if !state.Quota.NextRecoverAt.Equal(open) {
		t.Fatalf("NextRecoverAt = %v, want reused %v", state.Quota.NextRecoverAt, open)
	}
	if state.Quota.Reason != FailureCauseOverload {
		t.Fatalf("Reason = %q, want %q", state.Quota.Reason, FailureCauseOverload)
	}
}

func TestMarkResultCrossCauseReseedsBackoffLevel(t *testing.T) {
	withQuotaCooldownEnabled(t)

	t.Run("quota-to-overload", func(t *testing.T) {
		expired := time.Now().Add(-time.Second)
		manager := NewManager(nil, nil, nil)
		auth := &Auth{
			ID:       "auth-reseed-overload",
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
		if state.Quota.BackoffLevel != 1 {
			t.Fatalf("BackoffLevel = %d, want reseeded 1", state.Quota.BackoffLevel)
		}
		expectWindowAround(t, state.Quota.NextRecoverAt, overloadBackoffBase)
		if state.Quota.Reason != FailureCauseOverload {
			t.Fatalf("Reason = %q, want %q", state.Quota.Reason, FailureCauseOverload)
		}
	})

	t.Run("overload-to-quota", func(t *testing.T) {
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
	})
}

func TestMarkResultSuccessKeepsOpenCooldownWindow(t *testing.T) {
	withQuotaCooldownEnabled(t)

	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-overload-wipe", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	registerOverloadAuth(t, manager, auth)

	manager.MarkResult(context.Background(), overloadResult(auth.ID, "gpt-5"))
	armed := modelStateOrFatal(t, manager, auth.ID, "gpt-5")

	// An in-flight request admitted before the window was armed completes
	// successfully; its result is stale evidence and must not clear the window.
	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5", Success: true})
	state := modelStateOrFatal(t, manager, auth.ID, "gpt-5")
	if !state.Quota.NextRecoverAt.Equal(armed.Quota.NextRecoverAt) {
		t.Fatalf("NextRecoverAt = %v, want untouched %v", state.Quota.NextRecoverAt, armed.Quota.NextRecoverAt)
	}
	if !state.Quota.Exceeded || state.Quota.BackoffLevel != armed.Quota.BackoffLevel {
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

func TestMarkResultOverloadDisableCoolingLeavesNoWindow(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(true)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-overload-nocooling", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	registerOverloadAuth(t, manager, auth)

	manager.MarkResult(context.Background(), overloadResult(auth.ID, "gpt-5"))
	state := modelStateOrFatal(t, manager, auth.ID, "gpt-5")
	if !state.Quota.NextRecoverAt.IsZero() || !state.NextRetryAfter.IsZero() {
		t.Fatalf("expected no cooldown window with cooling disabled, got %+v", state.Quota)
	}
	if state.Quota.BackoffLevel != 0 {
		t.Fatalf("BackoffLevel = %d, want 0 with cooling disabled", state.Quota.BackoffLevel)
	}
	updated, _ := manager.GetByID(auth.ID)
	if blocked, _, _ := isAuthBlockedForModel(updated, "gpt-5", time.Now()); blocked {
		t.Fatal("expected model to stay available with cooling disabled")
	}
}
