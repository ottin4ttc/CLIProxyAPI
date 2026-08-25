package auth

import (
	"context"
	"errors"
	"sort"
	"time"

	log "github.com/sirupsen/logrus"
)

// healthRingPersisterLoop periodically snapshots every auth's ring and writes
// it through the configured store. The first tick fires immediately so a
// freshly restored ring reaches disk right away, which also narrows the
// own-port tie-break window after a restart.
func (m *Manager) healthRingPersisterLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = healthRingPersistInterval
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			m.persistHealthRings(ctx)
			timer.Reset(interval)
		}
	}
}

// persistHealthRings copies the raw ring of every registered auth under the
// manager lock, then serialises and writes outside it. A failed write is
// logged at warn and retried on the next tick.
func (m *Manager) persistHealthRings(ctx context.Context) {
	if m == nil {
		return
	}
	m.mu.RLock()
	store := m.healthRingStore
	if store == nil {
		m.mu.RUnlock()
		return
	}
	entries := make([]AuthRecentRequests, 0, len(m.auths))
	for id, auth := range m.auths {
		buckets := auth.exportRecentRequests()
		if len(buckets) == 0 {
			continue
		}
		entries = append(entries, AuthRecentRequests{ID: id, Buckets: buckets})
	}
	m.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	if errSave := store.Save(ctx, entries); errSave != nil && !errors.Is(errSave, context.Canceled) {
		log.Warnf("health ring: failed to persist state: %v", errSave)
	}
}
