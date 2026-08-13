package auth

import (
	"context"
	"testing"
	"time"
)

func TestMarkResultRecordsOverloadInRing(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-1", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.MarkResult(context.Background(), Result{AuthID: "auth-1", Provider: "codex", Model: "gpt-5", Success: true})
	mgr.MarkResult(context.Background(), Result{
		AuthID: "auth-1", Provider: "codex", Model: "gpt-5", Success: false,
		Error: &Error{Code: "rate_limit", Message: "server overloaded", HTTPStatus: 429, Cause: FailureCauseOverload},
	})
	mgr.MarkResult(context.Background(), Result{
		AuthID: "auth-1", Provider: "codex", Model: "gpt-5", Success: false,
		Error: &Error{Code: "quota", Message: "quota exhausted", HTTPStatus: 429},
	})

	gotAuth, ok := mgr.GetByID("auth-1")
	if !ok || gotAuth == nil {
		t.Fatalf("GetByID returned ok=%v auth=%v", ok, gotAuth)
	}
	snapshot := gotAuth.RecentRequestsSnapshot(time.Now())
	var successTotal, failedTotal, overloadTotal int64
	for _, bucket := range snapshot {
		successTotal += bucket.Success
		failedTotal += bucket.Failed
		overloadTotal += bucket.Overload
	}
	if successTotal != 1 || failedTotal != 2 || overloadTotal != 1 {
		t.Fatalf("totals = success=%d failed=%d overload=%d, want 1/2/1", successTotal, failedTotal, overloadTotal)
	}
}

func TestRecordRecentRequestOverloadBucketRollover(t *testing.T) {
	t.Parallel()

	auth := &Auth{ID: "a"}
	base := time.Unix(1_700_000_000, 0)
	auth.recordRecentRequest(base, false, true)
	// Same ring slot one full rotation later must reset all counters.
	later := base.Add(time.Duration(recentRequestBucketSeconds*recentRequestBucketCount) * time.Second)
	auth.recordRecentRequest(later, false, false)

	bucket := auth.recentRequests.buckets[recentRequestBucketIndex(recentRequestBucketID(later))]
	if bucket.failed != 1 || bucket.overload != 0 {
		t.Fatalf("bucket = %+v, want failed=1 overload=0", bucket)
	}
}

func TestOverloadWindowStatsExcludesCurrentBucket(t *testing.T) {
	t.Parallel()

	auth := &Auth{ID: "a"}
	now := time.Unix(1_700_000_000, 0)
	// In-progress bucket must be ignored.
	auth.recordRecentRequest(now, false, true)
	// Previous (completed) bucket counts.
	prev := now.Add(-time.Duration(recentRequestBucketSeconds) * time.Second)
	auth.recordRecentRequest(prev, true, false)
	auth.recordRecentRequest(prev, false, true)

	total, overload := auth.overloadWindowStats(now)
	if total != 2 || overload != 1 {
		t.Fatalf("stats = total=%d overload=%d, want 2/1", total, overload)
	}
}

func TestOverloadWindowStatsIgnoresBucketsOutsideWindow(t *testing.T) {
	t.Parallel()

	auth := &Auth{ID: "a"}
	now := time.Unix(1_700_000_000, 0)
	outside := now.Add(-time.Duration(recentRequestBucketSeconds*(healthWindowBuckets+1)) * time.Second)
	auth.recordRecentRequest(outside, false, true)
	inside := now.Add(-time.Duration(recentRequestBucketSeconds*healthWindowBuckets) * time.Second)
	auth.recordRecentRequest(inside, true, false)

	total, overload := auth.overloadWindowStats(now)
	if total != 1 || overload != 0 {
		t.Fatalf("stats = total=%d overload=%d, want 1/0", total, overload)
	}
}

func TestOverloadWindowStatsEmptyRing(t *testing.T) {
	t.Parallel()

	auth := &Auth{ID: "a"}
	total, overload := auth.overloadWindowStats(time.Unix(1_700_000_000, 0))
	if total != 0 || overload != 0 {
		t.Fatalf("stats = total=%d overload=%d, want 0/0", total, overload)
	}
}

func TestHealthTierBoundaries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		total, overload int64
		want            int64
	}{
		{"below min samples stays neutral", 19, 0, healthTierNeutral},
		{"zero overload boosts max", 20, 0, healthTierBoostMax},
		{"exactly 2 percent boosts", 100, 2, healthTierBoost},
		{"above 2 percent is neutral", 100, 3, healthTierNeutral},
		{"exactly 8 percent is neutral", 100, 8, healthTierNeutral},
		{"above 8 percent hits floor", 100, 9, healthTierFloor},
	}
	for _, tc := range cases {
		if got := healthTier(tc.total, tc.overload); got != tc.want {
			t.Fatalf("%s: healthTier(%d, %d) = %d, want %d", tc.name, tc.total, tc.overload, got, tc.want)
		}
	}
}

func TestApplyShareGuard(t *testing.T) {
	t.Parallel()

	// Exactly 3× fair share is not clamped; above it is.
	if got := applyShareGuard(healthTierBoostMax, 30, 100, 10); got != healthTierBoostMax {
		t.Fatalf("at cap: tier = %d, want %d", got, healthTierBoostMax)
	}
	if got := applyShareGuard(healthTierBoostMax, 31, 100, 10); got != healthTierNeutral {
		t.Fatalf("above cap: tier = %d, want %d", got, healthTierNeutral)
	}
	// Guard only clamps boosts, never tiers at or below neutral.
	if got := applyShareGuard(healthTierFloor, 90, 100, 10); got != healthTierFloor {
		t.Fatalf("floor untouched: tier = %d, want %d", got, healthTierFloor)
	}
	// Small pools never trigger (fair share 50%, cap 150%).
	if got := applyShareGuard(healthTierBoostMax, 60, 100, 2); got != healthTierBoostMax {
		t.Fatalf("small pool: tier = %d, want %d", got, healthTierBoostMax)
	}
}
