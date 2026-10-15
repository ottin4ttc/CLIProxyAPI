package cliproxy

import (
	"context"
	"maps"
	"reflect"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// reconcilePersistedFileAuth re-applies a persisted file auth from disk when the
// persist did not land in the manager. A file-event snapshot read before the
// persist can be applied after the persist's manager update; it bumps the
// generation, so the persist's own update is then rejected as stale while the
// file already holds it, and the auth would stay stale until the next edit.
func (s *Service) reconcilePersistedFileAuth(ctx context.Context, persisted *coreauth.Auth) {
	if s == nil || s.coreManager == nil || persisted == nil || persisted.ID == "" {
		return
	}
	if persisted.AuthSourceKind() != coreauth.AuthSourceFile {
		return
	}
	current, ok := s.coreManager.GetByID(persisted.ID)
	if !ok || current == nil || persistedMetadataApplied(current, persisted) {
		return
	}
	w := s.currentWatcher()
	if w == nil {
		return
	}
	path := persisted.Attributes[coreauth.AttributePath]
	if path == "" {
		path = persisted.Attributes[coreauth.AttributeSource]
	}
	if path == "" {
		return
	}
	auths, errSnapshot := w.SnapshotAuthFile(path)
	if errSnapshot != nil {
		log.Debugf("persisted auth %s not reconciled: file snapshot failed: %v", persisted.ID, errSnapshot)
		return
	}
	for _, fresh := range auths {
		if fresh == nil || fresh.ID != persisted.ID {
			continue
		}
		log.Debugf("persisted auth %s was overwritten by a stale snapshot; reapplying its file", persisted.ID)
		update := watcher.AuthUpdate{Action: watcher.AuthUpdateActionModify, ID: fresh.ID, Auth: fresh}
		if _, rev := w.DispatchPersistedAuthUpdateWithRevision(&update); rev > 0 {
			update.SetRevision(rev)
		}
		s.handleAuthUpdate(ctx, update)
		return
	}
}

// persistedMetadataApplied reports whether the manager holds the persisted file
// content, ignoring runtime-only metadata the manager merges in.
func persistedMetadataApplied(current, persisted *coreauth.Auth) bool {
	currentMeta := maps.Clone(current.Metadata)
	persistedMeta := maps.Clone(persisted.Metadata)
	delete(currentMeta, antigravityPersistedHintsMetaKey)
	delete(persistedMeta, antigravityPersistedHintsMetaKey)
	return len(currentMeta) == 0 && len(persistedMeta) == 0 || reflect.DeepEqual(currentMeta, persistedMeta)
}
