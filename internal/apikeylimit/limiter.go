// Package apikeylimit enforces per-client-API-key request rate limits.
//
// It holds counters only and never reads configuration: the caller resolves the
// limit and passes it in on every request, so a configuration hot-reload takes
// effect immediately with no invalidation step.
package apikeylimit

import (
	"sync"
	"time"
)

// windowSize is the sliding window the requests-per-minute cap is measured over.
// A sliding window is used rather than a fixed per-minute bucket because a fixed
// bucket lets a client push twice the cap across a minute boundary.
const windowSize = time.Minute

// Limiter enforces a sliding windowSize request-count window per key.
// The zero value is not usable; call New.
//
// Memory: windows is keyed only by keys that passed authentication, and
// authentication accepts only keys present in the configuration, so the map is
// bounded by the configured key count. Each entry holds at most one window of
// timestamps, bounded by that key's limit. Keys with no limit never allocate an
// entry. An idle key keeps its last window until the process exits; at the
// configured scale that is a few kilobytes, so there is no janitor.
type Limiter struct {
	mu sync.Mutex
	// windows maps a key to the unix-nano timestamps of its in-window requests.
	windows map[string][]int64
}

// New constructs an empty Limiter.
func New() *Limiter {
	return &Limiter{windows: make(map[string][]int64)}
}

// Allow records a request against key and reports whether it is within limit.
//
// A limit <= 0 means unlimited and short-circuits without touching any state.
//
// A rejected request is NOT recorded; recording rejections would let a client
// that ignores 429 hold its own window permanently full, turning a rate limit
// into a ban. retryAfter is the exact time until the oldest in-window request
// expires, so a single backoff is enough for the retry to succeed.
func (l *Limiter) Allow(key string, limit int, now time.Time) (bool, time.Duration) {
	if l == nil || key == "" || limit <= 0 {
		return true, 0
	}
	nanos := now.UnixNano()
	cutoff := nanos - int64(windowSize)
	l.mu.Lock()
	defer l.mu.Unlock()
	recent := l.windows[key]
	index := 0
	for index < len(recent) && recent[index] <= cutoff {
		index++
	}
	if index > 0 {
		recent = append(recent[:0], recent[index:]...)
	}
	if len(recent) >= limit {
		l.windows[key] = recent
		// Pruning left only timestamps newer than nanos-windowSize, so the
		// oldest one always expires in the future and this stays positive.
		return false, time.Duration(recent[0] + int64(windowSize) - nanos)
	}
	l.windows[key] = append(recent, nanos)
	return true, 0
}
