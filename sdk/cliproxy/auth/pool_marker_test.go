package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func poolMarkerHeaders(size int) http.Header {
	headers := make(http.Header)
	headers.Set(poolMarkerHeader, strings.Repeat("a", size))
	return headers
}

func TestObservePoolMarkerConfirmsFastPoolAfterStreak(t *testing.T) {
	t.Parallel()

	base := time.Unix(1_700_000_000, 0)
	auth := &Auth{ID: "a", Provider: "codex"}

	for i := 1; i < poolMarkerConfirmations; i++ {
		auth.observePoolMarker("codex", poolMarkerHeaders(292), base)
		if got := auth.poolTier(base); got != poolTierNeutral {
			t.Fatalf("poolTier after %d fast samples = %d, want neutral %d", i, got, poolTierNeutral)
		}
	}

	auth.observePoolMarker("codex", poolMarkerHeaders(292), base)
	if got := auth.poolTier(base); got != poolTierBoost {
		t.Fatalf("poolTier after streak = %d, want boost %d", got, poolTierBoost)
	}
}

func TestObservePoolMarkerSlowSampleClearsStreak(t *testing.T) {
	t.Parallel()

	base := time.Unix(1_700_000_000, 0)
	auth := &Auth{ID: "a", Provider: "codex"}
	for i := 0; i < poolMarkerConfirmations; i++ {
		auth.observePoolMarker("codex", poolMarkerHeaders(292), base)
	}

	// A single slow turn resets the streak: the upstream flip is immediate.
	auth.observePoolMarker("codex", poolMarkerHeaders(312), base)
	if got := auth.poolTier(base); got != poolTierNeutral {
		t.Fatalf("poolTier after slow sample = %d, want neutral %d", got, poolTierNeutral)
	}
}

func TestObservePoolMarkerIgnoresOtherProvidersAndMissingHeader(t *testing.T) {
	t.Parallel()

	base := time.Unix(1_700_000_000, 0)
	other := &Auth{ID: "a", Provider: "antigravity"}
	for i := 0; i < poolMarkerConfirmations; i++ {
		other.observePoolMarker("antigravity", poolMarkerHeaders(292), base)
	}
	if got := other.poolTier(base); got != poolTierNeutral {
		t.Fatalf("non-codex poolTier = %d, want neutral %d", got, poolTierNeutral)
	}

	auth := &Auth{ID: "b", Provider: "codex"}
	for i := 0; i < poolMarkerConfirmations; i++ {
		auth.observePoolMarker("codex", poolMarkerHeaders(292), base)
	}
	// A response that never reached the pool carries no marker and must not
	// clear a live streak.
	auth.observePoolMarker("codex", make(http.Header), base)
	if got := auth.poolTier(base); got != poolTierBoost {
		t.Fatalf("poolTier after header-less response = %d, want boost %d", got, poolTierBoost)
	}
}

func TestPoolTierDecaysAfterTTL(t *testing.T) {
	t.Parallel()

	base := time.Unix(1_700_000_000, 0)
	auth := &Auth{ID: "a", Provider: "codex"}
	for i := 0; i < poolMarkerConfirmations; i++ {
		auth.observePoolMarker("codex", poolMarkerHeaders(292), base)
	}

	if got := auth.poolTier(base.Add(poolMarkerTTL)); got != poolTierBoost {
		t.Fatalf("poolTier at TTL edge = %d, want boost %d", got, poolTierBoost)
	}
	if got := auth.poolTier(base.Add(poolMarkerTTL + time.Second)); got != poolTierNeutral {
		t.Fatalf("poolTier past TTL = %d, want neutral %d", got, poolTierNeutral)
	}
}

func TestHealthWeightedSelectorPrefersFastPool(t *testing.T) {
	t.Parallel()

	base := time.Unix(1_700_000_000, 0)
	prev := base.Add(-time.Duration(recentRequestBucketSeconds) * time.Second)

	fast := &Auth{ID: "fast", Provider: "codex"}
	slow := &Auth{ID: "slow", Provider: "codex"}
	// Equal health and equal share, so only the pool marker separates them.
	for i := 0; i < 10; i++ {
		fast.recordRecentRequest(prev, true, false)
		slow.recordRecentRequest(prev, true, false)
	}
	for i := 0; i < poolMarkerConfirmations; i++ {
		fast.observePoolMarker("codex", poolMarkerHeaders(292), base)
		slow.observePoolMarker("codex", poolMarkerHeaders(312), base)
	}

	selector := &HealthWeightedRoundRobinSelector{
		PoolMarkerWeighting: true,
		nowFn:               func() time.Time { return base },
	}
	counts := make(map[string]int)
	for i := 0; i < 30; i++ {
		got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, []*Auth{fast, slow})
		if errPick != nil {
			t.Fatalf("Pick() #%d error = %v", i, errPick)
		}
		counts[got.ID]++
	}
	// ×2 against ×1 → 2:1 split of 30 picks.
	if counts["fast"] != 20 || counts["slow"] != 10 {
		t.Fatalf("counts = %#v, want fast=20 slow=10", counts)
	}
}

func TestHealthWeightedSelectorPoolWeightingDisabledByDefault(t *testing.T) {
	t.Parallel()

	base := time.Unix(1_700_000_000, 0)
	prev := base.Add(-time.Duration(recentRequestBucketSeconds) * time.Second)

	fast := &Auth{ID: "fast", Provider: "codex"}
	slow := &Auth{ID: "slow", Provider: "codex"}
	for i := 0; i < 10; i++ {
		fast.recordRecentRequest(prev, true, false)
		slow.recordRecentRequest(prev, true, false)
	}
	for i := 0; i < poolMarkerConfirmations; i++ {
		fast.observePoolMarker("codex", poolMarkerHeaders(292), base)
		slow.observePoolMarker("codex", poolMarkerHeaders(312), base)
	}

	selector := &HealthWeightedRoundRobinSelector{nowFn: func() time.Time { return base }}
	counts := make(map[string]int)
	for i := 0; i < 30; i++ {
		got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, []*Auth{fast, slow})
		if errPick != nil {
			t.Fatalf("Pick() #%d error = %v", i, errPick)
		}
		counts[got.ID]++
	}
	if counts["fast"] != 15 || counts["slow"] != 15 {
		t.Fatalf("counts = %#v, want an even 15/15 split", counts)
	}
}

func poolMarkerFixture(t *testing.T, hotSamples, hotOverloads int) (*HealthWeightedRoundRobinSelector, []*Auth, time.Time) {
	t.Helper()

	base := time.Unix(1_700_000_000, 0)
	prev := base.Add(-time.Duration(recentRequestBucketSeconds) * time.Second)

	// The hot credential runs a low but non-zero overload rate so its health
	// tier stays neutral. That isolates the pool boost: the share guard never
	// touches a tier that is already neutral, so any withheld boost below is
	// the pool one.
	hot := &Auth{ID: "hot", Provider: "codex"}
	for i := 0; i < hotSamples-hotOverloads; i++ {
		hot.recordRecentRequest(prev, true, false)
	}
	for i := 0; i < hotOverloads; i++ {
		hot.recordRecentRequest(prev, false, true)
	}
	auths := []*Auth{hot}
	for i := 0; i < 3; i++ {
		quiet := &Auth{ID: "quiet-" + strconv.Itoa(i), Provider: "codex"}
		for j := 0; j < 10; j++ {
			quiet.recordRecentRequest(prev, true, false)
		}
		auths = append(auths, quiet)
	}

	for i := 0; i < poolMarkerConfirmations; i++ {
		hot.observePoolMarker("codex", poolMarkerHeaders(292), base)
		for _, quiet := range auths[1:] {
			quiet.observePoolMarker("codex", poolMarkerHeaders(312), base)
		}
	}

	selector := &HealthWeightedRoundRobinSelector{
		PoolMarkerWeighting: true,
		nowFn:               func() time.Time { return base },
	}
	return selector, auths, base
}

func poolMarkerPickCounts(t *testing.T, selector *HealthWeightedRoundRobinSelector, auths []*Auth, picks int) map[string]int {
	t.Helper()

	counts := make(map[string]int)
	for i := 0; i < picks; i++ {
		got, errPick := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
		if errPick != nil {
			t.Fatalf("Pick() #%d error = %v", i, errPick)
		}
		counts[got.ID]++
	}
	return counts
}

func TestHealthWeightedSelectorShareGuardWithholdsPoolBoost(t *testing.T) {
	t.Parallel()

	// 100 window attempts against a 130 attempt pool of 4: 100×4 > 130×3, so
	// the anti-avalanche guard withholds the fast-pool boost.
	selector, auths, _ := poolMarkerFixture(t, 100, 5)
	counts := poolMarkerPickCounts(t, selector, auths, 20)

	for _, auth := range auths {
		if counts[auth.ID] != 5 {
			t.Fatalf("counts = %#v, want an even 5 each (boost withheld)", counts)
		}
	}
}

func TestHealthWeightedSelectorAppliesPoolBoostBelowShareGuard(t *testing.T) {
	t.Parallel()

	// 30 window attempts against a 60 attempt pool of 4: 30×4 < 60×3, so the
	// guard stays out of the way and the ×2 fast-pool boost applies.
	selector, auths, _ := poolMarkerFixture(t, 30, 2)
	counts := poolMarkerPickCounts(t, selector, auths, 20)

	if counts["hot"] != 8 {
		t.Fatalf("counts = %#v, want hot=8", counts)
	}
	for _, auth := range auths[1:] {
		if counts[auth.ID] != 4 {
			t.Fatalf("counts = %#v, want 4 for each slow credential", counts)
		}
	}
}
