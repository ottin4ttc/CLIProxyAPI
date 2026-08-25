package auth

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
)

// SetHealthRingStore configures the health ring persistence backend.
func (m *Manager) SetHealthRingStore(store HealthRingStore) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.healthRingStore = store
	m.mu.Unlock()
}

// RestoreHealthRings loads the persisted recent-request rings and applies them
// to the registered auths. It is meant to run once at startup, before traffic:
// buckets outside the live window are dropped, unknown auth ids are skipped,
// and occupied ring slots are never overwritten, so even a warm ring cannot be
// clobbered. The caller only logs a returned error; restore never fails
// startup.
func (m *Manager) RestoreHealthRings(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	store := m.healthRingStore
	m.mu.RUnlock()
	if store == nil {
		return nil
	}
	entries, errLoad := store.Load(ctx)
	if errLoad != nil {
		return errLoad
	}
	if len(entries) == 0 {
		return nil
	}
	now := time.Now()
	restoredBuckets := 0
	restoredAuths := 0
	m.mu.Lock()
	for _, entry := range entries {
		auth := m.auths[entry.ID]
		if auth == nil {
			continue
		}
		if count := auth.importRecentRequests(now, entry.Buckets); count > 0 {
			restoredBuckets += count
			restoredAuths++
		}
	}
	m.mu.Unlock()
	if restoredAuths > 0 {
		log.Infof("health ring: restored %d bucket(s) across %d auth(s)", restoredBuckets, restoredAuths)
	}
	return nil
}

// StartHealthRingPersister launches the background persister loop. Only one
// loop is kept alive; starting a new one cancels the previous run. It no-ops
// when no health ring store is configured.
func (m *Manager) StartHealthRingPersister(parent context.Context) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.healthRingStore == nil {
		m.mu.Unlock()
		return
	}
	cancelPrev := m.healthRingCancel
	m.healthRingCancel = nil
	m.mu.Unlock()
	if cancelPrev != nil {
		cancelPrev()
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancelCtx := context.WithCancel(parent)
	m.mu.Lock()
	m.healthRingCancel = cancelCtx
	m.mu.Unlock()
	go m.healthRingPersisterLoop(ctx, healthRingPersistInterval)
}

// StopHealthRingPersister cancels the persister loop and flushes once, best
// effort, so a graceful shutdown loses at most the requests recorded since the
// flush. It is safe to call when the persister never started.
func (m *Manager) StopHealthRingPersister() {
	if m == nil {
		return
	}
	m.mu.Lock()
	cancel := m.healthRingCancel
	m.healthRingCancel = nil
	store := m.healthRingStore
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if store != nil {
		m.persistHealthRings(context.Background())
	}
}
