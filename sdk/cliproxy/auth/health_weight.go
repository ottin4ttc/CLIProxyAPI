package auth

import (
	"time"
)

const (
	// healthWindowBuckets is how many completed ring buckets feed the health
	// score (6 × 10 min = 60 minutes). The in-progress bucket is excluded so
	// the score — and therefore the weight vector — changes only on bucket
	// boundaries, keeping smooth weighted round-robin state stable in between.
	healthWindowBuckets = 6
	// healthMinSamples keeps low-traffic accounts neutral: below this many
	// attempts in the window the overload rate is noise.
	healthMinSamples = 20
	// Health tiers are ×2 fixed-point weight multipliers.
	healthTierBoostMax = 8 // ×4: zero overloads in the window
	healthTierBoost    = 4 // ×2: overload rate ≤ 2%
	healthTierNeutral  = 2 // ×1: overload rate ≤ 8% or insufficient samples
	healthTierFloor    = 1 // ×0.5: overload rate > 8%; never zero so probe traffic keeps flowing
	// healthShareCapMultiple caps a boosted account's window traffic share at
	// this multiple of fair share (poolTotal / poolSize) before the boost is
	// withdrawn. Anti-avalanche guard.
	healthShareCapMultiple = 3
)

// overloadWindowStats sums attempts and overload failures over the most
// recent completed ring buckets, excluding the in-progress bucket.
func (a *Auth) overloadWindowStats(now time.Time) (total, overload int64) {
	if a == nil {
		return 0, 0
	}
	currentBucketID := recentRequestBucketID(now)
	for i := 1; i <= healthWindowBuckets; i++ {
		bucketID := currentBucketID - int64(i)
		bucket := a.recentRequests.buckets[recentRequestBucketIndex(bucketID)]
		if bucket.bucketID != bucketID {
			continue
		}
		total += bucket.success + bucket.failed
		overload += bucket.overload
	}
	return total, overload
}

// healthTier maps completed-window overload stats to a fixed-point tier.
// Integer math only: rate ≤ N% is expressed as overload*100 <= total*N.
func healthTier(total, overload int64) int64 {
	if total < healthMinSamples {
		return healthTierNeutral
	}
	switch {
	case overload == 0:
		return healthTierBoostMax
	case overload*100 <= total*2:
		return healthTierBoost
	case overload*100 <= total*8:
		return healthTierNeutral
	default:
		return healthTierFloor
	}
}

// applyShareGuard withdraws a boost when the account already carries more
// than healthShareCapMultiple × fair share of the pool's window traffic.
// It never pushes a tier below neutral — demotion is the health signal's job.
func applyShareGuard(tier, total, poolTotal int64, poolSize int) int64 {
	if tier <= healthTierNeutral || poolSize <= 0 {
		return tier
	}
	if total*int64(poolSize) > poolTotal*healthShareCapMultiple {
		return healthTierNeutral
	}
	return tier
}
