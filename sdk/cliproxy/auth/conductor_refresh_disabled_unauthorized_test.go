package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestRefreshAuthForRequest_DisabledAuth_Unauthorized_NeverRetries(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &mockOAuthErrorExecutor{
		id: "test-provider",
		errToReturn: oauthStatusError{
			code: http.StatusUnauthorized,
			msg:  `{"error":{"message":"Your session has ended. Please log in again.","code":"refresh_token_invalidated"}}`,
		},
	}
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "disabled-unauthorized",
		Provider: "test-provider",
		Disabled: true,
		Status:   StatusDisabled,
		Metadata: map[string]any{
			"access_token":  "expired-token",
			"refresh_token": "refresh-1",
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register error: %v", err)
	}

	if _, errRefresh := manager.refreshAuthForRequest(ctx, auth.ID, ""); errRefresh == nil {
		t.Fatal("expected refresh error, got nil")
	}

	manager.mu.RLock()
	current := manager.auths[auth.ID].Clone()
	manager.mu.RUnlock()
	if current.Status != StatusDisabled {
		t.Fatalf("auth status = %v, want StatusDisabled", current.Status)
	}
	if !current.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %v, want zero time (never refresh)", current.NextRefreshAfter)
	}
	if _, ok := nextRefreshCheckAt(time.Now(), current, 15*time.Minute); ok {
		t.Fatal("disabled auth with a rejected refresh token is still scheduled for refresh")
	}

	callsBefore := executor.refreshCalls.Load()
	if _, errRefresh := manager.refreshAuthForRequest(ctx, auth.ID, ""); errRefresh == nil {
		t.Fatal("expected second refresh to be blocked")
	}
	if got := executor.refreshCalls.Load(); got != callsBefore {
		t.Fatalf("executor.Refresh called again (%d -> %d), want blocked", callsBefore, got)
	}
}

func TestRefreshAuthForRequest_DisabledAuth_TransientError_StillRetries(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &mockOAuthErrorExecutor{
		id:          "test-provider",
		errToReturn: oauthStatusError{code: http.StatusBadGateway, msg: "bad gateway"},
	}
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "disabled-transient",
		Provider: "test-provider",
		Disabled: true,
		Status:   StatusDisabled,
		Metadata: map[string]any{
			"access_token":  "expired-token",
			"refresh_token": "refresh-1",
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	if _, errRefresh := manager.refreshAuthForRequest(ctx, auth.ID, ""); errRefresh == nil {
		t.Fatal("expected refresh error, got nil")
	}

	manager.mu.RLock()
	current := manager.auths[auth.ID].Clone()
	manager.mu.RUnlock()
	if current.NextRefreshAfter.IsZero() {
		t.Fatal("NextRefreshAfter is zero, want a backoff retry for a transient refresh failure")
	}
}
