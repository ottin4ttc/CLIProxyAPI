package apikeylimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var base = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

func TestAllowUnderAndOverLimit(t *testing.T) {
	l := New()
	for i := 0; i < 3; i++ {
		ok, _ := l.Allow("sk-a", 3, base.Add(time.Duration(i)*time.Second))
		if !ok {
			t.Fatalf("request %d rejected below limit", i)
		}
	}
	ok, retryAfter := l.Allow("sk-a", 3, base.Add(3*time.Second))
	if ok {
		t.Fatal("4th request within the window should be rejected")
	}
	// The oldest request landed at base and leaves the window at base+60s,
	// which is 57s after this attempt.
	if retryAfter != 57*time.Second {
		t.Fatalf("retryAfter = %v, want 57s", retryAfter)
	}
}

func TestWindowSlidesAtExactlySixtySeconds(t *testing.T) {
	l := New()
	if ok, _ := l.Allow("sk-a", 1, base); !ok {
		t.Fatal("first request rejected")
	}
	if ok, _ := l.Allow("sk-a", 1, base.Add(59*time.Second)); ok {
		t.Fatal("request at +59s should still be inside the window")
	}
	if ok, _ := l.Allow("sk-a", 1, base.Add(60*time.Second)); !ok {
		t.Fatal("request at +60s should be allowed; the window is exclusive at the boundary")
	}
}

func TestRejectedRequestsAreNotRecorded(t *testing.T) {
	l := New()
	if ok, _ := l.Allow("sk-a", 1, base); !ok {
		t.Fatal("first request rejected")
	}
	// Hammer the limiter while throttled. If rejections were recorded, the
	// window would never drain and the key would be banned rather than limited.
	for i := 1; i <= 100; i++ {
		if ok, _ := l.Allow("sk-a", 1, base.Add(time.Duration(i)*time.Millisecond)); ok {
			t.Fatalf("request %d should have been rejected", i)
		}
	}
	if ok, _ := l.Allow("sk-a", 1, base.Add(60*time.Second)); !ok {
		t.Fatal("key should recover once the single recorded request expires")
	}
}

func TestZeroLimitIsUnlimited(t *testing.T) {
	l := New()
	for i := 0; i < 500; i++ {
		if ok, _ := l.Allow("sk-anon", 0, base.Add(time.Duration(i)*time.Millisecond)); !ok {
			t.Fatalf("request %d rejected under an unlimited key", i)
		}
	}
	// An unlimited key must not allocate state at all.
	if _, present := l.windows["sk-anon"]; present {
		t.Fatal("unlimited keys must not create a window entry")
	}
}

func TestKeysAreIsolated(t *testing.T) {
	l := New()
	if ok, _ := l.Allow("sk-a", 1, base); !ok {
		t.Fatal("sk-a first request rejected")
	}
	if ok, _ := l.Allow("sk-b", 1, base); !ok {
		t.Fatal("sk-b must have its own budget")
	}
	if ok, _ := l.Allow("sk-a", 1, base); ok {
		t.Fatal("sk-a should be over its own limit")
	}
}

func TestRetryAfterAlwaysPositive(t *testing.T) {
	// Pruning guarantees every surviving timestamp is newer than now-windowSize,
	// so the oldest one always expires in the future. A zero Retry-After would
	// tell the client to retry immediately into the same rejection.
	l := New()
	l.Allow("sk-a", 1, base)
	for _, offset := range []time.Duration{0, time.Millisecond, 30 * time.Second, 59999 * time.Millisecond} {
		ok, retryAfter := l.Allow("sk-a", 1, base.Add(offset))
		if ok {
			t.Fatalf("offset %v: expected rejection", offset)
		}
		if retryAfter <= 0 || retryAfter > windowSize {
			t.Fatalf("offset %v: retryAfter = %v, want within (0, %v]", offset, retryAfter, windowSize)
		}
	}
}

func TestConcurrentAllow(t *testing.T) {
	// All goroutines race against the same fixed now, so they all land in the
	// same window and compete for the same budget. If Allow lost an update
	// under concurrent access, more than limit calls would be allowed; if it
	// double-counted, fewer would be. Either failure mode is invisible without
	// this exact-count assertion, which is the whole point of the test.
	const goroutines = 200
	const limit = 37
	l := New()
	var wg sync.WaitGroup
	var allowed int64
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Allow("sk-a", limit, base); ok {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	wg.Wait()
	if allowed != limit {
		t.Fatalf("allowed = %d, want exactly %d", allowed, limit)
	}
}

func TestNilLimiterAllows(t *testing.T) {
	var l *Limiter
	if ok, _ := l.Allow("sk-a", 1, base); !ok {
		t.Fatal("nil limiter must allow")
	}
}
