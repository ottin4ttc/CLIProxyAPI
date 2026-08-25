package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func writeHealthRingStateFile(t *testing.T, dir string, port int, updatedAt time.Time, entries []AuthRecentRequests) string {
	t.Helper()
	file := healthRingFile{
		Version:       healthRingFileVersion,
		UpdatedAt:     updatedAt.UTC(),
		BucketSeconds: recentRequestBucketSeconds,
		Auths:         entries,
	}
	data, errMarshal := json.Marshal(file)
	if errMarshal != nil {
		t.Fatalf("marshal state file: %v", errMarshal)
	}
	path := filepath.Join(dir, healthRingFilePrefix+strconv.Itoa(port)+healthRingFileSuffix)
	if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
		t.Fatalf("write state file: %v", errWrite)
	}
	return path
}

func singleAuthEntries(id string, success int64) []AuthRecentRequests {
	return []AuthRecentRequests{{ID: id, Buckets: []PersistedRequestBucket{{BucketID: 2911037, Success: success}}}}
}

func loadedAuthID(t *testing.T, entries []AuthRecentRequests) string {
	t.Helper()
	if len(entries) != 1 {
		t.Fatalf("Load() returned %d entries, want 1", len(entries))
	}
	return entries[0].ID
}

func TestFileHealthRingStoreSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewFileHealthRingStore(dir, 1111)
	entries := []AuthRecentRequests{{
		ID: "auth-a",
		Buckets: []PersistedRequestBucket{
			{BucketID: 2911035, Success: 10, Failed: 2, Overload: 1},
			{BucketID: 2911036, Success: 7},
		},
	}}
	if errSave := store.Save(context.Background(), entries); errSave != nil {
		t.Fatalf("Save() returned error: %v", errSave)
	}
	if _, errStat := os.Stat(filepath.Join(dir, "recent-requests.1111.state")); errStat != nil {
		t.Fatalf("state file missing: %v", errStat)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
	loaded, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load() returned error: %v", errLoad)
	}
	if len(loaded) != 1 || loaded[0].ID != "auth-a" || len(loaded[0].Buckets) != 2 {
		t.Fatalf("Load() = %+v, want the saved entries", loaded)
	}
	if loaded[0].Buckets[0] != entries[0].Buckets[0] || loaded[0].Buckets[1] != entries[0].Buckets[1] {
		t.Fatalf("Load() buckets = %+v, want %+v", loaded[0].Buckets, entries[0].Buckets)
	}
}

func TestFileHealthRingStoreFreshestWins(t *testing.T) {
	now := time.Now().UTC()
	for name, newerPort := range map[string]int{"newer-first": 1111, "newer-second": 2222} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			olderPort := 3333
			writeHealthRingStateFile(t, dir, newerPort, now.Add(-5*time.Minute), singleAuthEntries("from-newer", 1))
			writeHealthRingStateFile(t, dir, olderPort, now.Add(-10*time.Minute), singleAuthEntries("from-older", 1))
			// Port 4444 has no own file: plain freshest-wins applies.
			store := NewFileHealthRingStore(dir, 4444)
			loaded, errLoad := store.Load(context.Background())
			if errLoad != nil {
				t.Fatalf("Load() returned error: %v", errLoad)
			}
			if got := loadedAuthID(t, loaded); got != "from-newer" {
				t.Fatalf("Load() picked %q, want the freshest file", got)
			}
		})
	}
}

func TestFileHealthRingStoreOwnPortTieBreak(t *testing.T) {
	now := time.Now().UTC()

	t.Run("foreign-within-one-tick-loses", func(t *testing.T) {
		dir := t.TempDir()
		writeHealthRingStateFile(t, dir, 1111, now.Add(-30*time.Second), singleAuthEntries("own", 1))
		writeHealthRingStateFile(t, dir, 2222, now, singleAuthEntries("foreign", 1))
		store := NewFileHealthRingStore(dir, 1111)
		loaded, errLoad := store.Load(context.Background())
		if errLoad != nil {
			t.Fatalf("Load() returned error: %v", errLoad)
		}
		if got := loadedAuthID(t, loaded); got != "own" {
			t.Fatalf("Load() picked %q, want the own-port file", got)
		}
	})

	t.Run("foreign-far-ahead-wins", func(t *testing.T) {
		dir := t.TempDir()
		writeHealthRingStateFile(t, dir, 1111, now.Add(-10*time.Minute), singleAuthEntries("own", 1))
		writeHealthRingStateFile(t, dir, 2222, now, singleAuthEntries("foreign", 1))
		store := NewFileHealthRingStore(dir, 1111)
		loaded, errLoad := store.Load(context.Background())
		if errLoad != nil {
			t.Fatalf("Load() returned error: %v", errLoad)
		}
		if got := loadedAuthID(t, loaded); got != "foreign" {
			t.Fatalf("Load() picked %q, want the foreign file", got)
		}
	})

	t.Run("no-own-file-uses-foreign", func(t *testing.T) {
		dir := t.TempDir()
		writeHealthRingStateFile(t, dir, 2222, now, singleAuthEntries("foreign", 1))
		store := NewFileHealthRingStore(dir, 1111)
		loaded, errLoad := store.Load(context.Background())
		if errLoad != nil {
			t.Fatalf("Load() returned error: %v", errLoad)
		}
		if got := loadedAuthID(t, loaded); got != "foreign" {
			t.Fatalf("Load() picked %q, want the foreign file", got)
		}
	})
}

func TestFileHealthRingStoreMalformedFallsBackToNextFreshest(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "recent-requests.2222.state")
	if errWrite := os.WriteFile(corrupt, []byte("{not json"), 0o600); errWrite != nil {
		t.Fatalf("write corrupt file: %v", errWrite)
	}
	writeHealthRingStateFile(t, dir, 3333, now.Add(-10*time.Minute), singleAuthEntries("valid-older", 1))
	store := NewFileHealthRingStore(dir, 1111)
	loaded, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load() returned error: %v", errLoad)
	}
	if got := loadedAuthID(t, loaded); got != "valid-older" {
		t.Fatalf("Load() picked %q, want the next-freshest valid file", got)
	}
}

func TestFileHealthRingStoreAllMalformedStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	if errWrite := os.WriteFile(filepath.Join(dir, "recent-requests.2222.state"), []byte("garbage"), 0o600); errWrite != nil {
		t.Fatalf("write corrupt file: %v", errWrite)
	}
	store := NewFileHealthRingStore(dir, 1111)
	loaded, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load() returned error: %v", errLoad)
	}
	if len(loaded) != 0 {
		t.Fatalf("Load() = %+v, want empty", loaded)
	}
}

func TestFileHealthRingStoreVersionAndBucketSecondsMismatch(t *testing.T) {
	now := time.Now().UTC()
	for name, mutate := range map[string]func(*healthRingFile){
		"version":        func(f *healthRingFile) { f.Version = healthRingFileVersion + 1 },
		"bucket-seconds": func(f *healthRingFile) { f.BucketSeconds = recentRequestBucketSeconds / 2 },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			file := healthRingFile{
				Version:       healthRingFileVersion,
				UpdatedAt:     now,
				BucketSeconds: recentRequestBucketSeconds,
				Auths:         singleAuthEntries("mismatch", 1),
			}
			mutate(&file)
			data, errMarshal := json.Marshal(file)
			if errMarshal != nil {
				t.Fatalf("marshal: %v", errMarshal)
			}
			if errWrite := os.WriteFile(filepath.Join(dir, "recent-requests.1111.state"), data, 0o600); errWrite != nil {
				t.Fatalf("write: %v", errWrite)
			}
			store := NewFileHealthRingStore(dir, 1111)
			loaded, errLoad := store.Load(context.Background())
			if errLoad != nil {
				t.Fatalf("Load() returned error: %v", errLoad)
			}
			if len(loaded) != 0 {
				t.Fatalf("Load() = %+v, want empty on mismatch", loaded)
			}
		})
	}
}

func TestFileHealthRingStoreCleanup(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()

	staleState := writeHealthRingStateFile(t, dir, 2222, now.Add(-healthRingSpan-time.Hour), singleAuthEntries("stale", 1))
	freshState := writeHealthRingStateFile(t, dir, 3333, now.Add(-5*time.Minute), singleAuthEntries("fresh", 1))

	corruptOld := filepath.Join(dir, "recent-requests.4444.state")
	if errWrite := os.WriteFile(corruptOld, []byte("junk"), 0o600); errWrite != nil {
		t.Fatalf("write corrupt file: %v", errWrite)
	}
	oldMtime := now.Add(-healthRingSpan - time.Hour)
	if errChtimes := os.Chtimes(corruptOld, oldMtime, oldMtime); errChtimes != nil {
		t.Fatalf("chtimes: %v", errChtimes)
	}
	corruptFresh := filepath.Join(dir, "recent-requests.5555.state")
	if errWrite := os.WriteFile(corruptFresh, []byte("junk"), 0o600); errWrite != nil {
		t.Fatalf("write corrupt file: %v", errWrite)
	}

	orphanTemp := filepath.Join(dir, "recent-requests.9999.state.123.tmp")
	if errWrite := os.WriteFile(orphanTemp, []byte("half"), 0o600); errWrite != nil {
		t.Fatalf("write orphan temp: %v", errWrite)
	}
	if errChtimes := os.Chtimes(orphanTemp, oldMtime, oldMtime); errChtimes != nil {
		t.Fatalf("chtimes: %v", errChtimes)
	}
	freshTemp := filepath.Join(dir, "recent-requests.9998.state.456.tmp")
	if errWrite := os.WriteFile(freshTemp, []byte("half"), 0o600); errWrite != nil {
		t.Fatalf("write fresh temp: %v", errWrite)
	}

	store := NewFileHealthRingStore(dir, 1111)
	if errSave := store.Save(context.Background(), singleAuthEntries("own", 1)); errSave != nil {
		t.Fatalf("Save() returned error: %v", errSave)
	}

	for _, gone := range []string{staleState, corruptOld, orphanTemp} {
		if _, errStat := os.Stat(gone); !os.IsNotExist(errStat) {
			t.Fatalf("expected %s to be cleaned up, stat err = %v", filepath.Base(gone), errStat)
		}
	}
	for _, kept := range []string{freshState, corruptFresh, freshTemp, store.filePath()} {
		if _, errStat := os.Stat(kept); errStat != nil {
			t.Fatalf("expected %s to survive cleanup: %v", filepath.Base(kept), errStat)
		}
	}
}
