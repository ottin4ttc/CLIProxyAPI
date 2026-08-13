package auth

import (
	"context"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
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

// HealthWeightedRoundRobinSelector is smooth weighted round-robin whose
// effective weights scale each credential's static weight by a health tier
// derived from its recent overload rate. Tiers are recomputed from completed
// ring buckets only, so the weight vector is stable between bucket boundaries.
type HealthWeightedRoundRobinSelector struct {
	mu        sync.Mutex
	states    map[string]*smoothWeightedState
	lastTiers map[string]int64
	maxKeys   int
	// nowFn overrides time.Now in tests.
	nowFn func() time.Time
}

type authHealthStat struct {
	total    int64
	overload int64
}

func (s *HealthWeightedRoundRobinSelector) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

// Pick selects the next available auth using smooth weighted round-robin
// over health-adjusted weights (static weight × share-guarded health tier).
func (s *HealthWeightedRoundRobinSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := s.now()
	available, errAvailable := getAvailableAuths(positiveWeightAuths(auths), provider, model, now)
	if errAvailable != nil {
		return nil, errAvailable
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	stats := make(map[string]authHealthStat, len(available))
	var poolTotal int64
	for _, auth := range available {
		total, overload := auth.overloadWindowStats(now)
		stats[auth.ID] = authHealthStat{total: total, overload: overload}
		poolTotal += total
	}

	stateModel := weightedSelectorStateModel(ctx, model)
	key := provider + ":" + canonicalModelKey(stateModel)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = make(map[string]*smoothWeightedState)
	}
	if s.lastTiers == nil {
		s.lastTiers = make(map[string]int64)
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}
	if _, ok := s.states[key]; !ok && len(s.states) >= limit {
		s.states = make(map[string]*smoothWeightedState)
	}
	state := s.states[key]
	if state == nil {
		state = &smoothWeightedState{}
		s.states[key] = state
	}

	weights := make(map[string]int64, len(available))
	for _, auth := range available {
		stat := stats[auth.ID]
		tier := healthTier(stat.total, stat.overload)
		guarded := applyShareGuard(tier, stat.total, poolTotal, len(available))
		if guarded != tier {
			log.Infof("health-weight: share guard | auth=%s window_total=%d pool_total=%d pool_size=%d tier %d->%d",
				auth.ID, stat.total, poolTotal, len(available), tier, guarded)
		}
		if last := s.lastTiers[auth.ID]; last != guarded {
			log.Infof("health-weight: tier change | auth=%s %d->%d window_total=%d window_overload=%d",
				auth.ID, last, guarded, stat.total, stat.overload)
			s.lastTiers[auth.ID] = guarded
		}
		if weight := authWeight(auth); weight > 0 {
			weights[auth.ID] = weight * guarded
		}
	}

	state.prepare(weights)
	picked := pickSmoothWeightedAuth(available, state.current, func(auth *Auth) int64 { return weights[auth.ID] })
	if picked == nil {
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available with positive weight"}
	}
	return picked, nil
}
