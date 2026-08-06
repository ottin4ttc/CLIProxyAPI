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

func TestArchiveIdleCompressesOldActives(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "label", "old.jsonl")
	fresh := filepath.Join(dir, "label", "fresh.jsonl")
	writeAged(t, active, "{\"ts\":1}\n", 8*time.Hour)
	writeAged(t, fresh, "{\"ts\":2}\n", time.Minute)
	if err := ArchiveIdle(dir, time.Now().Add(-6*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(active); !os.IsNotExist(err) {
		t.Fatal("old active file should be removed after archiving")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh file must be untouched")
	}
	if got := zstdCat(t, active+".zst"); got != "{\"ts\":1}\n" {
		t.Fatalf("archive content = %q", got)
	}
}

func TestArchiveIdleAppendsFrameWhenArchiveExists(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "label", "sess.jsonl")
	// First archive round.
	writeAged(t, active, "{\"ts\":1}\n", 8*time.Hour)
	if err := ArchiveIdle(dir, time.Now().Add(-6*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	// Session resumed, went idle again.
	writeAged(t, active, "{\"ts\":2}\n", 8*time.Hour)
	if err := ArchiveIdle(dir, time.Now().Add(-6*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if got := zstdCat(t, active+".zst"); got != "{\"ts\":1}\n{\"ts\":2}\n" {
		t.Fatalf("concatenated frames = %q", got)
	}
}

// TestArchiveIdleSkipsPendingPath verifies the skip callback lets the
// maintenance loop leave a file with an in-flight writer append untouched:
// no archive is created and the plaintext file survives unmodified.
func TestArchiveIdleSkipsPendingPath(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "label", "old.jsonl")
	writeAged(t, active, "{\"ts\":1}\n", 8*time.Hour)

	skip := func(path string) bool { return path == active }
	if err := ArchiveIdle(dir, time.Now().Add(-6*time.Hour), skip); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(active)
	if err != nil {
		t.Fatalf("skipped file should remain: %v", err)
	}
	if string(data) != "{\"ts\":1}\n" {
		t.Fatalf("skipped file content changed: %q", data)
	}
	if _, err := os.Stat(active + ".zst"); !os.IsNotExist(err) {
		t.Fatal("skipped file must not be archived")
	}
}

// TestArchiveAndRemoveRollsBackOnEncodeFailure drives archiveAndRemove's
// encode path into an error (source is a directory, so io.Copy fails) and
// verifies the archive is left exactly as found rather than gaining a torn
// frame, and the source is not removed.
func TestArchiveAndRemoveRollsBackOnEncodeFailure(t *testing.T) {
	dir := t.TempDir()

	// A source file that is really a directory: os.Open succeeds but every
	// Read fails, so appendZstdFrame's io.Copy fails without ever writing a
	// byte through the zstd encoder for this attempt.
	sourceDir := filepath.Join(dir, "broken.jsonl")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Seed the archive with one real, valid frame first so there is
	// pre-existing content that must survive the failed second attempt
	// untouched.
	realSource := filepath.Join(dir, "seed.jsonl")
	if err := os.WriteFile(realSource, []byte("{\"ts\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	archivePath := sourceDir + ".zst"
	if err := appendZstdFrame(archivePath, realSource); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := archiveAndRemove(sourceDir); err == nil {
		t.Fatal("expected archiveAndRemove to fail when the source is a directory")
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
	if _, err := os.Stat(sourceDir); err != nil {
		t.Fatal("source must not be removed after a failed append")
	}
}

// TestRollbackArchiveRestoresRecordedSize is the minimum-bar regression for
// the rollback helper itself: append a real frame, record the archive's
// size, simulate a failed second append leaving extra bytes behind, then
// roll back. The archive must come back byte-identical to before the failed
// attempt and still decode correctly.
func TestRollbackArchiveRestoresRecordedSize(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "sess.jsonl")
	if err := os.WriteFile(source, []byte("{\"ts\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	archivePath := source + ".zst"
	if err := appendZstdFrame(archivePath, source); err != nil {
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
