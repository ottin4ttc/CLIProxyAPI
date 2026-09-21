package auth

import (
	"context"
	"strings"
	"sync"
)

// credentialInFlightLimiter counts requests currently executing per
// credential and lets callers wait for the next release.
type credentialInFlightLimiter struct {
	mu     sync.Mutex
	active map[string]int
	// wake is closed and replaced on every release so a refused caller can
	// block until any slot frees.
	wake chan struct{}
}

func newCredentialInFlightLimiter() *credentialInFlightLimiter {
	return &credentialInFlightLimiter{active: make(map[string]int), wake: make(chan struct{})}
}

// Acquire reserves one slot for authID while fewer than limit are active. On
// success it returns an idempotent release function; on refusal it returns
// the channel that closes at the next release anywhere in the limiter, so
// the caller can wait without missing a release that lands in between.
func (l *credentialInFlightLimiter) Acquire(authID string, limit int) (func(), <-chan struct{}, bool) {
	if l == nil || limit <= 0 {
		return nil, nil, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[authID] >= limit {
		return nil, l.wake, false
	}
	l.active[authID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.active[authID] <= 1 {
				delete(l.active, authID)
			} else {
				l.active[authID]--
			}
			close(l.wake)
			l.wake = make(chan struct{})
		})
	}, nil, true
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
// to auth; the release function is nil when no cap applies, and the wake
// channel is only set when the slot was refused.
func (m *Manager) acquireCredentialInFlight(auth *Auth, provider string) (func(), <-chan struct{}, bool) {
	if m == nil || auth == nil {
		return nil, nil, true
	}
	limit := m.credentialInFlightLimit(provider)
	if limit <= 0 {
		return nil, nil, true
	}
	return m.credentialInFlight.Acquire(auth.ID, limit)
}

// waitCredentialInFlight blocks until wake fires (a slot was released) or the
// request context ends. It is the only place a request waits on the cap: the
// request has not reached an upstream yet, so this is credential acquisition
// time, not an upstream timeout.
func waitCredentialInFlight(ctx context.Context, wake <-chan struct{}, busyCount int) error {
	logEntryWithRequestID(ctx).Debugf("all %d candidate credentials at in-flight cap, waiting for a slot", busyCount)
	if ctx == nil {
		<-wake
		return nil
	}
	select {
	case <-wake:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
