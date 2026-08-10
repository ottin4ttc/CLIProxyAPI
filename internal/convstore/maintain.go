package convstore

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// ArchiveIdle compresses active session files whose mtime is older than
// idleBefore into "<name>.zst" and removes the plaintext original. When the
// archive already exists (session resumed after a previous archive round),
// a new zstd frame is appended — concatenated frames decode as one stream.
//
// skip, when non-nil, is consulted for every candidate path; a true result
// leaves the file untouched. This lets callers exclude files with in-flight
// writer appends: archiving (and removing) a file whose writer queue still
// holds a pending line for it would race the writer recreating the file,
// producing non-monotonic turn numbers. Pass nil to skip nothing.
func ArchiveIdle(dataDir string, idleBefore time.Time, skip func(path string) bool) error {
	return walkSuffix(dataDir, ".jsonl", func(path string, info os.FileInfo) error {
		if skip != nil && skip(path) {
			return nil
		}
		if !info.ModTime().Before(idleBefore) {
			return nil
		}
		if err := archiveAndRemove(path); err != nil {
			return fmt.Errorf("archive %s: %w", path, err)
		}
		return nil
	})
}

// PurgeArchives removes archived sessions older than olderThan.
func PurgeArchives(dataDir string, olderThan time.Time) error {
	return walkSuffix(dataDir, ".jsonl.zst", func(path string, info os.FileInfo) error {
		if !info.ModTime().Before(olderThan) {
			return nil
		}
		return os.Remove(path)
	})
}

func walkSuffix(root, suffix string, fn func(path string, info os.FileInfo) error) error {
	var firstErr error
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, suffix) {
			return nil
		}
		// ".jsonl" must not match ".jsonl.zst" files.
		if suffix == ".jsonl" && strings.HasSuffix(path, ".jsonl.zst") {
			return nil
		}
		if errFn := fn(path, info); errFn != nil && firstErr == nil {
			firstErr = errFn
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	return firstErr
}

// archiveAndRemove appends sourcePath's content as a new zstd frame to
// "<sourcePath>.zst" and, only on success, removes sourcePath. It owns the
// rollback for every partial-failure mode of that pair:
//
//   - encode failure (or a failed close of the encoder/archive): a torn,
//     undecodable frame could otherwise be left in the archive under
//     O_APPEND, permanently blocking decode of everything after it;
//   - a failed os.Remove(sourcePath) after a successful compress: without
//     rollback, the next archive round would re-append the same lines as a
//     duplicate frame, since the plaintext source is still on disk.
//
// Both are handled the same way: record the archive's size before writing,
// and on any failure in the sequence, truncate the archive back to that
// size so it is left exactly as it was found.
func archiveAndRemove(sourcePath string) error {
	archivePath := sourcePath + ".zst"
	startSize, err := archiveSize(archivePath)
	if err != nil {
		return err
	}
	if err := appendZstdFrame(archivePath, sourcePath); err != nil {
		rollbackArchive(archivePath, startSize)
		return err
	}
	if err := os.Remove(sourcePath); err != nil {
		rollbackArchive(archivePath, startSize)
		return err
	}
	return nil
}

// archiveSize returns archivePath's current size, or 0 if it does not exist
// yet (the first archive round for a session).
func archiveSize(archivePath string) (int64, error) {
	info, err := os.Stat(archivePath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return info.Size(), nil
}

// rollbackArchive truncates the archive back to a previously recorded size,
// discarding any bytes written by a failed append attempt.
func rollbackArchive(archivePath string, size int64) {
	if err := os.Truncate(archivePath, size); err != nil {
		fmt.Fprintf(os.Stderr, "[conversation-store] rollback truncate %s: %v\n", archivePath, err)
	}
}

// appendZstdFrame compresses sourcePath and appends the result as a new
// zstd frame onto archivePath (created if absent). It performs no rollback
// on its own; archiveAndRemove owns that.
func appendZstdFrame(archivePath, sourcePath string) error {
	src, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer func() {
		if errClose := src.Close(); errClose != nil {
			fmt.Fprintf(os.Stderr, "[conversation-store] close %s: %v\n", sourcePath, errClose)
		}
	}()
	dst, err := os.OpenFile(archivePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	enc, err := zstd.NewWriter(dst)
	if err != nil {
		_ = dst.Close()
		return err
	}
	_, errCopy := io.Copy(enc, src)
	if errClose := enc.Close(); errCopy == nil {
		errCopy = errClose
	}
	if errClose := dst.Close(); errCopy == nil {
		errCopy = errClose
	}
	return errCopy
}

// StartMaintenance runs the periodic loop: idle/grace expiry every 10s
// (ended streams land as "ok" within grace + one tick), archive and
// retention roughly hourly. It returns when stop is closed.
func (s *Store) StartMaintenance(stop <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	tick := 0
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.ExpireIdle()
			tick++
			if tick%360 != 0 {
				continue
			}
			s.mu.Lock()
			cfg := s.cfg
			s.mu.Unlock()
			idleBefore := s.now().Add(-time.Duration(cfg.ArchiveIdleHours) * time.Hour)
			if cfg.ArchiveIdleHours > 0 {
				if err := ArchiveIdle(cfg.DataDir, idleBefore, s.writer.HasPending); err != nil {
					fmt.Fprintf(os.Stderr, "[conversation-store] archive: %v\n", err)
				}
			}
			if cfg.RetentionDays > 0 {
				olderThan := s.now().Add(-time.Duration(cfg.RetentionDays) * 24 * time.Hour)
				if err := PurgeArchives(cfg.DataDir, olderThan); err != nil {
					fmt.Fprintf(os.Stderr, "[conversation-store] purge: %v\n", err)
				}
			}
		}
	}
}
