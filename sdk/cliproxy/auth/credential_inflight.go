package auth

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// credentialInFlightRetryAfter is the Retry-After advertised when every
// candidate credential is at its in-flight cap. Slots free up as soon as a
// running request ends, so a short hint keeps clients from backing off longer
// than the wait usually is.
const credentialInFlightRetryAfter = time.Second

// credentialInFlightLimiter counts requests currently executing per
// credential.
type credentialInFlightLimiter struct {
	mu     sync.Mutex
	active map[string]int
}

func newCredentialInFlightLimiter() *credentialInFlightLimiter {
	return &credentialInFlightLimiter{active: make(map[string]int)}
}

// Acquire reserves one slot for authID while fewer than limit are active. The
// returned release function is idempotent and nil when the slot was refused.
func (l *credentialInFlightLimiter) Acquire(authID string, limit int) (func(), bool) {
	if l == nil || limit <= 0 {
		return nil, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[authID] >= limit {
		return nil, false
	}
	l.active[authID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.active[authID] <= 1 {
				delete(l.active, authID)
				return
			}
			l.active[authID]--
		})
	}, true
}

// Active reports how many requests authID is currently serving.
func (l *credentialInFlightLimiter) Active(authID string) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active[authID]
}

// credentialInFlightLimit returns the configured in-flight cap for provider,
// or 0 when the provider is unlimited. Home owns credential admission, so the
// cap never applies in Home mode.
func (m *Manager) credentialInFlightLimit(provider string) int {
	if m == nil || m.HomeEnabled() {
		return 0
	}
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil || len(cfg.CredentialMaxInFlight) == 0 {
		return 0
	}
	key := strings.ToLower(strings.TrimSpace(provider))
	for name, limit := range cfg.CredentialMaxInFlight {
		if strings.ToLower(strings.TrimSpace(name)) != key {
			continue
		}
		if limit > 0 {
			return limit
		}
		return 0
	}
	return 0
}

// acquireCredentialInFlight reserves an execution slot on auth under the
// provider's in-flight cap. It reports whether the request may be dispatched
// to auth; the release function is nil when no cap applies.
func (m *Manager) acquireCredentialInFlight(auth *Auth, provider string) (func(), bool) {
	if m == nil || auth == nil {
		return nil, true
	}
	limit := m.credentialInFlightLimit(provider)
	if limit <= 0 {
		return nil, true
	}
	return m.credentialInFlight.Acquire(auth.ID, limit)
}

// newCredentialInFlightExceededError is returned when every otherwise
// available credential is at its in-flight cap: the request is rejected at
// the proxy instead of reaching an upstream account. It reuses the concurrency
// busy error shape so the existing Retry-After plumbing and the no-wait-retry
// short circuit in shouldRetryAfterError both apply.
func newCredentialInFlightExceededError() error {
	return newHomeConcurrencyBusyError(&Error{
		Code:       "credential_inflight_exceeded",
		Message:    "credential in-flight limit exceeded",
		Retryable:  true,
		HTTPStatus: http.StatusTooManyRequests,
	}, credentialInFlightRetryAfter)
}
