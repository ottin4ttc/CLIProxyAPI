package auth

import (
	"net/http"
	"strings"
	"time"
)

// Codex answers from more than one upstream serving pool and reveals which one
// handled the turn in the X-Codex-Turn-State response header. The payload is
// opaque and is never echoed back upstream, but its length is stable per pool:
// 292 bytes from the fast pool against 312 from the slow one. Measured over six
// hours of production traffic the fast pool returned a TTFT p50 of 6.7s against
// 14.9s for the slow one, so routing prefers a credential currently served
// there.
//
// Pool membership is assigned upstream and drifts on its own - mostly during
// the daily reassignment window, but a credential has been observed flipping
// back within three minutes - so this is a short-lived observation rather than
// a credential attribute, and it decays back to neutral once it goes stale.
const (
	poolMarkerHeader = "X-Codex-Turn-State"
	// poolMarkerFastMaxBytes splits the fast marker from the slow one. The
	// midpoint of the two observed lengths absorbs small upstream shifts.
	poolMarkerFastMaxBytes = 302
	// poolMarkerConfirmations is how many consecutive fast observations are
	// required before the boost applies. A single sample is not trusted
	// because a flip can bounce inside one minute.
	poolMarkerConfirmations = 3
	// poolMarkerTTL retires an observation that stopped being refreshed, so a
	// credential that went idle cannot keep a stale boost.
	poolMarkerTTL = 20 * time.Minute
	// Pool tiers reuse the health tier fixed-point scale so applyShareGuard
	// guards them unchanged: poolTierBoost is ×2 over poolTierNeutral.
	poolTierBoost   = healthTierBoost
	poolTierNeutral = healthTierNeutral
)

// poolMarkerState tracks the recent pool observations for one credential.
type poolMarkerState struct {
	fastStreak int
	observedAt time.Time
}

// ProviderSupportsPoolMarker reports whether a provider exposes the pool marker
// header. Only Codex does; every other provider observes nothing.
func ProviderSupportsPoolMarker(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "codex")
}

// observePoolMarker folds one upstream response into the credential's pool
// observation. Responses without the header leave the current state untouched,
// so a failed attempt that never reached the pool cannot clear a live streak.
func (a *Auth) observePoolMarker(provider string, headers http.Header, observedAt time.Time) {
	if a == nil || headers == nil || !ProviderSupportsPoolMarker(provider) {
		return
	}
	value := strings.TrimSpace(headers.Get(poolMarkerHeader))
	if value == "" {
		return
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	if len(value) <= poolMarkerFastMaxBytes {
		if a.poolMarker.fastStreak < poolMarkerConfirmations {
			a.poolMarker.fastStreak++
		}
	} else {
		a.poolMarker.fastStreak = 0
	}
	a.poolMarker.observedAt = observedAt
}

// poolTier reports the credential's pool multiplier on the health tier scale.
// It stays neutral until the streak confirms the fast pool, and returns to
// neutral once the last observation is older than poolMarkerTTL.
func (a *Auth) poolTier(now time.Time) int64 {
	if a == nil || a.poolMarker.fastStreak < poolMarkerConfirmations {
		return poolTierNeutral
	}
	if a.poolMarker.observedAt.IsZero() || now.Sub(a.poolMarker.observedAt) > poolMarkerTTL {
		return poolTierNeutral
	}
	return poolTierBoost
}

// PoolTier exposes the credential's current pool tier for reporting.
func (a *Auth) PoolTier(now time.Time) int64 {
	return a.poolTier(now)
}
