package convstore

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// ArchiveIdle compresses session directories whose mtime is older than
// idleBefore into "<sessionDir>.jsonl.zst" and removes the directory. When
// the archive already exists (session resumed after a previous archive
// round), a new zstd frame is appended — concatenated frames decode as one
// stream.
//
// skip, when non-nil, is consulted for every candidate directory; a true
// result leaves it untouched. This lets callers exclude directories with
// in-flight writer records: archiving and removing a directory the writer is
// about to add a file to would drop that record. Pass nil to skip nothing.
func ArchiveIdle(dataDir string, idleBefore time.Time, skip func(dir string) bool) error {
	keyEntries, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, keyEntry := range keyEntries {
		if !keyEntry.IsDir() {
			continue
		}
		keyDir := filepath.Join(dataDir, keyEntry.Name())
		sessionEntries, errRead := os.ReadDir(keyDir)
		if errRead != nil {
			if firstErr == nil {
				firstErr = errRead
			}
			continue
		}
		for _, sessionEntry := range sessionEntries {
			if !sessionEntry.IsDir() {
				continue
			}
			sessionDir := filepath.Join(keyDir, sessionEntry.Name())
			if skip != nil && skip(sessionDir) {
				continue
			}
			info, errInfo := sessionEntry.Info()
			if errInfo != nil {
				if firstErr == nil {
					firstErr = errInfo
				}
				continue
			}
			if !info.ModTime().Before(idleBefore) {
				continue
			}
			if errArchive := archiveDirAndRemove(sessionDir); errArchive != nil && firstErr == nil {
				firstErr = fmt.Errorf("archive %s: %w", sessionDir, errArchive)
			}
		}
	}
	return firstErr
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

// archiveDirAndRemove concatenates sessionDir's records as a new zstd frame
// on "<sessionDir>.jsonl.zst" and, only on success, removes the directory.
// It owns the rollback for every partial-failure mode of that pair:
//
//   - encode failure (or a failed close of the encoder/archive): a torn,
//     undecodable frame could otherwise be left in the archive under
//     O_APPEND, permanently blocking decode of everything after it;
//   - a failed removal after a successful compress: without rollback, the
//     next archive round would re-append the same records as a duplicate
//     frame, since the directory is still on disk.
//
// Both are handled the same way: record the archive's size before writing,
// and on any failure in the sequence, truncate the archive back to that
// size so it is left exactly as it was found.
func archiveDirAndRemove(sessionDir string) error {
	archivePath := sessionDir + ".jsonl.zst"
	startSize, err := archiveSize(archivePath)
	if err != nil {
		return err
	}
	if err := appendZstdFrameFromDir(archivePath, sessionDir); err != nil {
		rollbackArchive(archivePath, startSize)
		return err
	}
	if err := os.RemoveAll(sessionDir); err != nil {
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

// appendZstdFrameFromDir compresses every record in sessionDir, in file-name
// order, and appends the result as a single new zstd frame onto archivePath
// (created if absent). File names carry a zero-padded millisecond prefix, so
// name order is chronological order. Records are read one at a time, so peak
// memory is one record rather than the whole session. It performs no
// rollback on its own; archiveDirAndRemove owns that.
func appendZstdFrameFromDir(archivePath, sessionDir string) error {
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)

	dst, err := os.OpenFile(archivePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	enc, errEnc := zstd.NewWriter(dst)
	if errEnc != nil {
		if errClose := dst.Close(); errClose != nil {
			fmt.Fprintf(os.Stderr, "[conversation-store] close %s: %v\n", archivePath, errClose)
		}
		return errEnc
	}
	var errCopy error
	for _, name := range names {
		path := filepath.Join(sessionDir, name)
		src, errOpen := os.Open(path)
		if errOpen != nil {
			errCopy = errOpen
			break
		}
		_, errCopy = io.Copy(enc, src)
		if errClose := src.Close(); errCopy == nil {
			errCopy = errClose
		}
		if errCopy != nil {
			break
		}
	}
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
