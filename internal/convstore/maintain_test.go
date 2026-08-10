package convstore

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func writeAged(t *testing.T, path, content string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func zstdCat(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	out, err := io.ReadAll(dec)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestArchiveDirAndRemoveRollsBackOnEncodeFailure drives
// archiveDirAndRemove's encode path into an error: one record entry is a
// symlink to a directory, so os.Open follows it but the subsequent io.Copy
// read fails. It verifies the archive is left exactly as found rather than
// gaining a torn frame, and the session directory is not removed.
func TestArchiveDirAndRemoveRollsBackOnEncodeFailure(t *testing.T) {
	dir := t.TempDir()

	sessionDir := filepath.Join(dir, "key1", "broken-sess")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A record entry that is really a symlink to a directory: os.Open
	// follows it and succeeds, but every Read fails, so
	// appendZstdFrameFromDir's io.Copy fails without ever writing a
	// complete frame for this attempt.
	targetDir := filepath.Join(dir, "not-a-record")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetDir, filepath.Join(sessionDir, "0000000000001-a.jsonl")); err != nil {
		t.Fatal(err)
	}

	// Seed the archive with one real, valid frame first so there is
	// pre-existing content that must survive the failed second attempt
	// untouched.
	seedDir := filepath.Join(dir, "seed-sess")
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, "0000000000001-a.jsonl"), []byte("{\"ts\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	archivePath := sessionDir + ".jsonl.zst"
	if err := appendZstdFrameFromDir(archivePath, seedDir); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := archiveDirAndRemove(sessionDir); err == nil {
		t.Fatal("expected archiveDirAndRemove to fail when a record entry is a directory")
	}

	after, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("archive changed after failed append: before=%q after=%q", before, after)
	}
	if got := zstdCat(t, archivePath); got != "{\"ts\":1}\n" {
		t.Fatalf("archive must still decode to its pre-failure content, got %q", got)
	}
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatal("session dir must not be removed after a failed append")
	}
}

// TestRollbackArchiveRestoresRecordedSize is the minimum-bar regression for
// the rollback helper itself: append a real frame, record the archive's
// size, simulate a failed second append leaving extra bytes behind, then
// roll back. The archive must come back byte-identical to before the failed
// attempt and still decode correctly.
func TestRollbackArchiveRestoresRecordedSize(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "sess")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "0000000000001-a.jsonl"), []byte("{\"ts\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	archivePath := sessionDir + ".jsonl.zst"
	if err := appendZstdFrameFromDir(archivePath, sessionDir); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	size, err := archiveSize(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(before)) {
		t.Fatalf("archiveSize = %d, want %d", size, len(before))
	}

	// Simulate a torn frame left behind by a failed append: extra bytes
	// appended past the recorded size.
	f, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("garbage-partial-frame")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	rollbackArchive(archivePath, size)

	after, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("rollback did not restore byte-identical archive: before=%q after=%q", before, after)
	}
	if got := zstdCat(t, archivePath); got != "{\"ts\":1}\n" {
		t.Fatalf("archive must still decode after rollback, got %q", got)
	}
}

func TestArchiveIdleMergesSessionDirInOrder(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "key1", "sess-a")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Written out of order on purpose; the archiver must sort by name.
	records := []struct{ name, body string }{
		{"0000000000010-c.jsonl", `{"ts":10}`},
		{"0000000000001-a.jsonl", `{"ts":1}`},
		{"0000000000002-b.jsonl", `{"ts":2}`},
	}
	for _, rec := range records {
		if err := os.WriteFile(filepath.Join(sessionDir, rec.name), []byte(rec.body+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rec.name, err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(sessionDir, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if err := ArchiveIdle(dir, time.Now(), nil); err != nil {
		t.Fatalf("archive: %v", err)
	}

	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Fatalf("session dir still present after archive: %v", err)
	}
	got := zstdCat(t, sessionDir+".jsonl.zst")
	want := "{\"ts\":1}\n{\"ts\":2}\n{\"ts\":10}\n"
	if got != want {
		t.Fatalf("archive content = %q, want %q", got, want)
	}
}

func TestArchiveIdleSkipsDirWithPendingWrite(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "key1", "sess-b")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "0000000000001-a.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(sessionDir, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	skip := func(d string) bool { return d == sessionDir }
	if err := ArchiveIdle(dir, time.Now(), skip); err != nil {
		t.Fatalf("archive: %v", err)
	}

	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("skipped session dir was archived anyway: %v", err)
	}
	if _, err := os.Stat(sessionDir + ".jsonl.zst"); !os.IsNotExist(err) {
		t.Fatal("skipped session dir produced an archive")
	}
}

func TestArchiveIdleLeavesFreshDirAlone(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "key1", "sess-c")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "0000000000001-a.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := ArchiveIdle(dir, time.Now().Add(-time.Hour), nil); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("fresh session dir was archived: %v", err)
	}
}

func TestArchiveIdleAppendsFrameOnResume(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "key1", "sess-d")
	archivePath := sessionDir + ".jsonl.zst"
	old := time.Now().Add(-2 * time.Hour)

	for _, body := range []string{`{"round":1}`, `{"round":2}`} {
		if err := os.MkdirAll(sessionDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(sessionDir, "0000000000001-a.jsonl"), []byte(body+"\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Chtimes(sessionDir, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		if err := ArchiveIdle(dir, time.Now(), nil); err != nil {
			t.Fatalf("archive: %v", err)
		}
	}

	got := zstdCat(t, archivePath)
	want := "{\"round\":1}\n{\"round\":2}\n"
	if got != want {
		t.Fatalf("archive content = %q, want %q", got, want)
	}
}

func TestPurgeArchives(t *testing.T) {
	dir := t.TempDir()
	oldArchive := filepath.Join(dir, "label", "ancient.jsonl.zst")
	newArchive := filepath.Join(dir, "label", "recent.jsonl.zst")
	writeAged(t, oldArchive, "x", 15*24*time.Hour)
	writeAged(t, newArchive, "x", 24*time.Hour)
	if err := PurgeArchives(dir, time.Now().Add(-14*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldArchive); !os.IsNotExist(err) {
		t.Fatal("old archive should be purged")
	}
	if _, err := os.Stat(newArchive); err != nil {
		t.Fatal("recent archive must remain")
	}
}
