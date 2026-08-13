package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
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

func TestHealthWeightedSelectorBoostsImmuneAuth(t *testing.T) {
	t.Parallel()

	base := time.Unix(1_700_000_000, 0)
	prev := base.Add(-time.Duration(recentRequestBucketSeconds) * time.Second)

	immune := &Auth{ID: "immune"}
	sick := &Auth{ID: "sick"}
	for i := 0; i < 100; i++ {
		immune.recordRecentRequest(prev, true, false)
	}
	for i := 0; i < 90; i++ {
		sick.recordRecentRequest(prev, true, false)
	}
	for i := 0; i < 10; i++ {
		sick.recordRecentRequest(prev, false, true) // 10% overload → floor tier
	}

	selector := &HealthWeightedRoundRobinSelector{nowFn: func() time.Time { return base }}
	counts := make(map[string]int)
	for i := 0; i < 90; i++ {
		got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, []*Auth{immune, sick})
		if errPick != nil {
			t.Fatalf("Pick() #%d error = %v", i, errPick)
		}
		counts[got.ID]++
	}
	// Tiers 8 (×4) vs 1 (×0.5) → 8:1 split of 90 picks.
	if counts["immune"] != 80 || counts["sick"] != 10 {
		t.Fatalf("counts = %#v, want immune=80 sick=10", counts)
	}
}

func TestHealthWeightedSelectorNeutralWhenInsufficientSamples(t *testing.T) {
	t.Parallel()

	base := time.Unix(1_700_000_000, 0)
	prev := base.Add(-time.Duration(recentRequestBucketSeconds) * time.Second)

	quiet := &Auth{ID: "quiet"}
	noisy := &Auth{ID: "noisy"}
	for i := 0; i < 10; i++ { // below healthMinSamples
		quiet.recordRecentRequest(prev, true, false)
	}
	for i := 0; i < 5; i++ {
		noisy.recordRecentRequest(prev, true, false)
		noisy.recordRecentRequest(prev, false, true) // 50% overload but only 10 samples
	}

	selector := &HealthWeightedRoundRobinSelector{nowFn: func() time.Time { return base }}
	counts := make(map[string]int)
	for i := 0; i < 20; i++ {
		got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, []*Auth{quiet, noisy})
		if errPick != nil {
			t.Fatalf("Pick() #%d error = %v", i, errPick)
		}
		counts[got.ID]++
	}
	if counts["quiet"] != 10 || counts["noisy"] != 10 {
		t.Fatalf("counts = %#v, want 10/10 (both neutral)", counts)
	}
}

func TestHealthWeightedSelectorSkipsZeroStaticWeight(t *testing.T) {
	t.Parallel()

	base := time.Unix(1_700_000_000, 0)
	zero := &Auth{ID: "zero", Attributes: map[string]string{AttributeWeight: "0"}}
	normal := &Auth{ID: "normal"}

	selector := &HealthWeightedRoundRobinSelector{nowFn: func() time.Time { return base }}
	for i := 0; i < 10; i++ {
		got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, []*Auth{zero, normal})
		if errPick != nil {
			t.Fatalf("Pick() #%d error = %v", i, errPick)
		}
		if got.ID != "normal" {
			t.Fatalf("Pick() #%d = %q, want %q", i, got.ID, "normal")
		}
	}
}

func TestHealthWeightedSelectorShareGuardClampsBoost(t *testing.T) {
	t.Parallel()

	base := time.Unix(1_700_000_000, 0)
	prev := base.Add(-time.Duration(recentRequestBucketSeconds) * time.Second)

	hot := &Auth{ID: "hot"}
	for i := 0; i < 400; i++ { // 80% of pool traffic, zero overload
		hot.recordRecentRequest(prev, true, false)
	}
	others := make([]*Auth, 0, 4)
	pool := []*Auth{hot}
	for _, id := range []string{"o1", "o2", "o3", "o4"} {
		other := &Auth{ID: id}
		for i := 0; i < 25; i++ {
			other.recordRecentRequest(prev, true, false)
		}
		others = append(others, other)
		pool = append(pool, other)
	}

	selector := &HealthWeightedRoundRobinSelector{nowFn: func() time.Time { return base }}
	counts := make(map[string]int)
	for i := 0; i < 34; i++ {
		got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, pool)
		if errPick != nil {
			t.Fatalf("Pick() #%d error = %v", i, errPick)
		}
		counts[got.ID]++
	}
	// hot: tier 8 clamped to 2 (400×5 > 500×3); others keep tier 8.
	// Weights 2 + 4×8 = 34 → hot gets 2 picks, each other 8.
	if counts["hot"] != 2 {
		t.Fatalf("hot picks = %d, want 2 (boost clamped)", counts["hot"])
	}
	for _, other := range others {
		if counts[other.ID] != 8 {
			t.Fatalf("%s picks = %d, want 8", other.ID, counts[other.ID])
		}
	}
}

func TestHealthWeightedSelectorReconvergesAfterWindowAges(t *testing.T) {
	t.Parallel()

	base := time.Unix(1_700_000_000, 0)
	prev := base.Add(-time.Duration(recentRequestBucketSeconds) * time.Second)

	immune := &Auth{ID: "immune"}
	sick := &Auth{ID: "sick"}
	for i := 0; i < 100; i++ {
		immune.recordRecentRequest(prev, true, false)
	}
	for i := 0; i < 80; i++ {
		sick.recordRecentRequest(prev, true, false)
	}
	for i := 0; i < 20; i++ {
		sick.recordRecentRequest(prev, false, true)
	}

	current := base
	selector := &HealthWeightedRoundRobinSelector{nowFn: func() time.Time { return current }}
	for i := 0; i < 50; i++ {
		if _, errPick := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, []*Auth{immune, sick}); errPick != nil {
			t.Fatalf("warmup Pick() #%d error = %v", i, errPick)
		}
	}

	// Advance beyond the window: all buckets age out, everyone back to neutral.
	current = base.Add(70 * time.Minute)
	counts := make(map[string]int)
	for i := 0; i < 20; i++ {
		got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, []*Auth{immune, sick})
		if errPick != nil {
			t.Fatalf("Pick() #%d error = %v", i, errPick)
		}
		counts[got.ID]++
	}
	if counts["immune"] != 10 || counts["sick"] != 10 {
		t.Fatalf("counts = %#v, want 10/10 after window aged out", counts)
	}
}

func TestSessionAffinityWithHealthFallbackSkipsZeroWeight(t *testing.T) {
	t.Parallel()

	fallback := &HealthWeightedRoundRobinSelector{}
	selector := NewSessionAffinitySelector(fallback)
	defer selector.Stop()

	zero := &Auth{ID: "zero", Attributes: map[string]string{AttributeWeight: "0"}}
	normal := &Auth{ID: "normal"}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"session-1"}}}

	for i := 0; i < 2; i++ {
		got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{zero, normal})
		if errPick != nil {
			t.Fatalf("Pick() #%d error = %v", i, errPick)
		}
		if got.ID != "normal" {
			t.Fatalf("Pick() #%d = %q, want %q", i, got.ID, "normal")
		}
	}
}

func TestAuthHealthTierExported(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	prev := now.Add(-time.Duration(recentRequestBucketSeconds) * time.Second)

	clean := &Auth{ID: "clean"}
	for i := 0; i < 25; i++ {
		clean.recordRecentRequest(prev, true, false)
	}
	if got := clean.HealthTier(now); got != healthTierBoostMax {
		t.Fatalf("HealthTier(clean) = %d, want %d", got, healthTierBoostMax)
	}

	empty := &Auth{ID: "empty"}
	if got := empty.HealthTier(now); got != healthTierNeutral {
		t.Fatalf("HealthTier(empty) = %d, want %d", got, healthTierNeutral)
	}
}
