package auth

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeHealthRingStore struct {
	mu      sync.Mutex
	load    []AuthRecentRequests
	loadErr error
	saved   [][]AuthRecentRequests
}

func (s *fakeHealthRingStore) Load(context.Context) ([]AuthRecentRequests, error) {
	return s.load, s.loadErr
}

func (s *fakeHealthRingStore) Save(_ context.Context, entries []AuthRecentRequests) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, entries)
	return nil
}

func (s *fakeHealthRingStore) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saved)
}

// windowBuckets builds ≥ healthMinSamples of clean traffic inside the
// completed health window relative to now.
func windowBuckets(now time.Time) []PersistedRequestBucket {
	currentID := recentRequestBucketID(now)
	return []PersistedRequestBucket{
		{BucketID: currentID - 1, Success: 15},
		{BucketID: currentID - 2, Success: 15},
	}
}

func TestManagerRestoreHealthRingsMutatesAuths(t *testing.T) {
	now := time.Now()
	store := &fakeHealthRingStore{load: []AuthRecentRequests{
		{ID: "auth-1", Buckets: windowBuckets(now)},
		{ID: "ghost", Buckets: windowBuckets(now)},
	}}
	manager := NewManager(nil, nil, nil)
	manager.SetHealthRingStore(store)
	ctx := WithSkipPersist(context.Background())
	if _, errRegister := manager.Register(ctx, &Auth{ID: "auth-1", Provider: "codex"}); errRegister != nil {
		t.Fatalf("Register() returned error: %v", errRegister)
	}
	if _, errRegister := manager.Register(ctx, &Auth{ID: "auth-2", Provider: "codex"}); errRegister != nil {
		t.Fatalf("Register() returned error: %v", errRegister)
	}

	if errRestore := manager.RestoreHealthRings(context.Background()); errRestore != nil {
		t.Fatalf("RestoreHealthRings() returned error: %v", errRestore)
	}

	restored, ok := manager.GetByID("auth-1")
	if !ok {
		t.Fatal("auth-1 not found after restore")
	}
	if got := restored.HealthTier(now); got != healthTierBoostMax {
		t.Fatalf("restored HealthTier = %d, want %d", got, healthTierBoostMax)
	}
	untouched, ok := manager.GetByID("auth-2")
	if !ok {
		t.Fatal("auth-2 not found")
	}
	if got := untouched.HealthTier(now); got != healthTierNeutral {
		t.Fatalf("auth-2 HealthTier = %d, want neutral %d", got, healthTierNeutral)
	}
}

func TestManagerRestoreHealthRingsWithoutStoreIsNoop(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	if errRestore := manager.RestoreHealthRings(context.Background()); errRestore != nil {
		t.Fatalf("RestoreHealthRings() without store returned error: %v", errRestore)
	}
}

func TestManagerStartHealthRingPersisterWithoutStoreIsNoop(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.StartHealthRingPersister(context.Background())
	manager.mu.RLock()
	cancel := manager.healthRingCancel
	manager.mu.RUnlock()
	if cancel != nil {
		t.Fatal("persister started without a store")
	}
	manager.StopHealthRingPersister()
}

func TestManagerHealthRingPersisterWritesFile(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(nil, nil, nil)
	manager.SetHealthRingStore(NewFileHealthRingStore(dir, 1111))

	seed := &Auth{ID: "auth-1", Provider: "codex"}
	seed.recordRecentRequest(time.Now(), true, false)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), seed); errRegister != nil {
		t.Fatalf("Register() returned error: %v", errRegister)
	}

	manager.StartHealthRingPersister(context.Background())
	defer manager.StopHealthRingPersister()

	path := filepath.Join(dir, "recent-requests.1111.state")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, errStat := os.Stat(path); errStat == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("persister did not write the state file")
		}
		time.Sleep(10 * time.Millisecond)
	}

	loaded, errLoad := NewFileHealthRingStore(dir, 1111).Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load() returned error: %v", errLoad)
	}
	if len(loaded) != 1 || loaded[0].ID != "auth-1" || len(loaded[0].Buckets) != 1 || loaded[0].Buckets[0].Success != 1 {
		t.Fatalf("persisted state = %+v, want auth-1 with one success bucket", loaded)
	}
}

func TestManagerStopHealthRingPersisterFlushes(t *testing.T) {
	store := &fakeHealthRingStore{}
	manager := NewManager(nil, nil, nil)
	manager.SetHealthRingStore(store)

	seed := &Auth{ID: "auth-1", Provider: "codex"}
	seed.recordRecentRequest(time.Now(), true, false)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), seed); errRegister != nil {
		t.Fatalf("Register() returned error: %v", errRegister)
	}

	manager.StopHealthRingPersister()
	if got := store.saveCount(); got != 1 {
		t.Fatalf("StopHealthRingPersister() saved %d times, want 1 flush", got)
	}
}

func TestManagerHealthRingPersistConcurrentWithRecording(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(nil, nil, nil)
	manager.SetHealthRingStore(NewFileHealthRingStore(dir, 1111))
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: "auth-1", Provider: "codex"}); errRegister != nil {
		t.Fatalf("Register() returned error: %v", errRegister)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			manager.mu.Lock()
			if auth := manager.auths["auth-1"]; auth != nil {
				auth.recordRecentRequest(time.Now(), true, false)
			}
			manager.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			manager.persistHealthRings(context.Background())
		}
	}()
	wg.Wait()
}
