package apikeylimit

import "sync"

var (
	defaultOnce    sync.Once
	defaultLimiter *Limiter
)

// Default returns the process-wide Limiter shared by every caller that needs
// the per-client-API-key RPM budget: the HTTP rpmLimitMiddleware
// (internal/api) and the WebSocket generation-dispatch path
// (sdk/api/handlers/openai) both call Default() so a client is throttled
// against the same counter no matter which transport it used, instead of
// each transport keeping its own independent budget. Lazily initialized,
// mirroring sdk/cliproxy/usage's DefaultManager()/PublishRecord shape, so
// importing this package never allocates until the limiter is actually
// needed.
//
// New() remains exported for tests that need an isolated limiter instead of
// the shared instance.
func Default() *Limiter {
	defaultOnce.Do(func() {
		defaultLimiter = New()
	})
	return defaultLimiter
}
