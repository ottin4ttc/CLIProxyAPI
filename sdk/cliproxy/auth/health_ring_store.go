package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	healthRingFileVersion = 1
	// healthRingPersistInterval is the persister tick. It doubles as the
	// own-port tie-break tolerance: a foreign slot file leading the own-port
	// file by no more than one tick is considered near-simultaneous, and a
	// process trusts its own last write over a near-simultaneous foreign one.
	healthRingPersistInterval = 60 * time.Second
	// healthRingSpan is the ring's full coverage; a state file older than this
	// contains nothing restorable.
	healthRingSpan       = time.Duration(recentRequestBucketSeconds*int64(recentRequestBucketCount)) * time.Second
	healthRingFilePrefix = "recent-requests."
	healthRingFileSuffix = ".state"
	// healthRingTempMaxAge is how old an orphaned temp file must be before the
	// cleanup sweep removes it; a live writer's temp exists only for an instant.
	healthRingTempMaxAge = 10 * time.Minute
)

// HealthRingStore persists the recent-request health ring independently from auth tokens.
type HealthRingStore interface {
	Load(context.Context) ([]AuthRecentRequests, error)
	Save(context.Context, []AuthRecentRequests) error
}

type healthRingFile struct {
	Version       int                  `json:"version"`
	UpdatedAt     time.Time            `json:"updated_at"`
	BucketSeconds int64                `json:"bucket_seconds"`
	Auths         []AuthRecentRequests `json:"auths"`
}

// FileHealthRingStore stores the whole pool's ring state as one
// recent-requests.<port>.state file per slot inside the auth directory.
// The temp pattern used for atomic replacement neither matches the state-file
// glob nor ends in .json, so neither file class is visible to the auth watcher.
type FileHealthRingStore struct {
	dir  string
	port int
	// now overrides time.Now in tests.
	now func() time.Time
}

// NewFileHealthRingStore creates a file-backed health ring store rooted at dir
// for the slot listening on port.
func NewFileHealthRingStore(dir string, port int) *FileHealthRingStore {
	return &FileHealthRingStore{dir: strings.TrimSpace(dir), port: port}
}

func (s *FileHealthRingStore) timeNow() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *FileHealthRingStore) filePath() string {
	return filepath.Join(s.dir, healthRingFilePrefix+strconv.Itoa(s.port)+healthRingFileSuffix)
}

func (s *FileHealthRingStore) stateGlob() string {
	return filepath.Join(s.dir, healthRingFilePrefix+"*"+healthRingFileSuffix)
}

// Save atomically replaces this slot's state file and opportunistically cleans
// up stale sibling files.
func (s *FileHealthRingStore) Save(ctx context.Context, entries []AuthRecentRequests) error {
	if s == nil || s.dir == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return errCtx
	}
	envelope := healthRingFile{
		Version:       healthRingFileVersion,
		UpdatedAt:     s.timeNow().UTC(),
		BucketSeconds: recentRequestBucketSeconds,
		Auths:         entries,
	}
	data, errMarshal := json.Marshal(envelope)
	if errMarshal != nil {
		return fmt.Errorf("marshal health ring state: %w", errMarshal)
	}
	data = append(data, '\n')

	path := s.filePath()
	tmpFile, errCreate := os.CreateTemp(s.dir, filepath.Base(path)+".*.tmp")
	if errCreate != nil {
		return fmt.Errorf("create health ring temp file: %w", errCreate)
	}
	tmp := tmpFile.Name()
	if _, errWrite := tmpFile.Write(data); errWrite != nil {
		if errClose := tmpFile.Close(); errClose != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("write health ring temp file: %w; close temp file: %v", errWrite, errClose)
		}
		_ = os.Remove(tmp)
		return fmt.Errorf("write health ring temp file: %w", errWrite)
	}
	if errClose := tmpFile.Close(); errClose != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close health ring temp file: %w", errClose)
	}
	if errRename := os.Rename(tmp, path); errRename != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace health ring state file: %w", errRename)
	}
	s.cleanupStale()
	return nil
}

// Load reads every slot's state file and returns the content of the single
// freshest one by updated_at, applying the own-port tie-break. Malformed files
// are skipped with a warning so selection falls to the next-freshest valid
// file; a version or bucket_seconds mismatch on the selected file yields an
// empty result. Load never merges files.
func (s *FileHealthRingStore) Load(ctx context.Context) ([]AuthRecentRequests, error) {
	if s == nil || s.dir == "" {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return nil, errCtx
	}
	matches, errGlob := filepath.Glob(s.stateGlob())
	if errGlob != nil {
		return nil, fmt.Errorf("glob health ring state files: %w", errGlob)
	}
	candidates := make([]healthRingCandidate, 0, len(matches))
	for _, path := range matches {
		data, errRead := os.ReadFile(path)
		if errRead != nil {
			if !errors.Is(errRead, os.ErrNotExist) {
				log.Warnf("health ring: failed to read state file %s: %v", filepath.Base(path), errRead)
			}
			continue
		}
		var file healthRingFile
		if errUnmarshal := json.Unmarshal(data, &file); errUnmarshal != nil {
			log.Warnf("health ring: skipping malformed state file %s: %v", filepath.Base(path), errUnmarshal)
			continue
		}
		candidates = append(candidates, healthRingCandidate{path: path, file: file})
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	chosen := pickHealthRingCandidate(candidates, s.filePath())
	if chosen.file.Version != healthRingFileVersion || chosen.file.BucketSeconds != recentRequestBucketSeconds {
		log.Warnf("health ring: state file %s has version=%d bucket_seconds=%d (want %d/%d), starting empty",
			filepath.Base(chosen.path), chosen.file.Version, chosen.file.BucketSeconds,
			healthRingFileVersion, recentRequestBucketSeconds)
		return nil, nil
	}
	return chosen.file.Auths, nil
}

type healthRingCandidate struct {
	path string
	file healthRingFile
}

// pickHealthRingCandidate selects the candidate with the newest updated_at.
// Own-port tie-break: when the newest foreign file leads the own-port file by
// no more than one persist tick, the own-port file is preferred. This keeps an
// outgoing slot's shutdown flush from beating the live slot's last tick, and
// gives second-granularity timestamp ties a defined outcome.
func pickHealthRingCandidate(candidates []healthRingCandidate, ownPath string) healthRingCandidate {
	newest := candidates[0]
	ownIndex := -1
	for i := range candidates {
		if candidates[i].file.UpdatedAt.After(newest.file.UpdatedAt) {
			newest = candidates[i]
		}
		if candidates[i].path == ownPath {
			ownIndex = i
		}
	}
	if ownIndex >= 0 && newest.path != ownPath &&
		newest.file.UpdatedAt.Sub(candidates[ownIndex].file.UpdatedAt) <= healthRingPersistInterval {
		return candidates[ownIndex]
	}
	return newest
}

// cleanupStale removes sibling state files whose content has fully aged out of
// the ring span (falling back to mtime when a file cannot be parsed, so a
// corrupt file does not survive forever) and orphaned temp files left behind
// by a crash between CreateTemp and rename.
func (s *FileHealthRingStore) cleanupStale() {
	now := s.timeNow()
	ownPath := s.filePath()
	matches, errGlob := filepath.Glob(s.stateGlob())
	if errGlob != nil {
		log.Warnf("health ring: failed to glob state files for cleanup: %v", errGlob)
		matches = nil
	}
	for _, path := range matches {
		if path == ownPath {
			continue
		}
		stamp, ok := healthRingFileStamp(path)
		if !ok {
			info, errStat := os.Stat(path)
			if errStat != nil {
				continue
			}
			stamp = info.ModTime()
		}
		if now.Sub(stamp) > healthRingSpan {
			if errRemove := os.Remove(path); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
				log.Warnf("health ring: failed to remove stale state file %s: %v", filepath.Base(path), errRemove)
			}
		}
	}
	tempMatches, errGlobTemp := filepath.Glob(filepath.Join(s.dir, healthRingFilePrefix+"*.tmp"))
	if errGlobTemp != nil {
		log.Warnf("health ring: failed to glob temp files for cleanup: %v", errGlobTemp)
		return
	}
	for _, path := range tempMatches {
		info, errStat := os.Stat(path)
		if errStat != nil {
			continue
		}
		if now.Sub(info.ModTime()) > healthRingTempMaxAge {
			if errRemove := os.Remove(path); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
				log.Warnf("health ring: failed to remove orphaned temp file %s: %v", filepath.Base(path), errRemove)
			}
		}
	}
}

// healthRingFileStamp reads the embedded updated_at of a state file.
func healthRingFileStamp(path string) (time.Time, bool) {
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return time.Time{}, false
	}
	var file healthRingFile
	if errUnmarshal := json.Unmarshal(data, &file); errUnmarshal != nil || file.UpdatedAt.IsZero() {
		return time.Time{}, false
	}
	return file.UpdatedAt, true
}
