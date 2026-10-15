package cliproxy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// A file-event snapshot read before a management persist can be applied after
// that persist's manager update. It bumps the generation, so the persist hook's
// own update is then rejected as stale; the hook must fall back to the file.
func TestRuntimeAuthSyncHookReconcilesAfterStaleFileSnapshot(t *testing.T) {
	authDir := t.TempDir()
	fileName := "codex-persist-reconcile.json"
	filePath := filepath.Join(authDir, fileName)
	writeAuthFile := func(prefix string) {
		content := fmt.Sprintf(`{"type":"codex","email":"user@example.com","access_token":"token","prefix":%q}`, prefix)
		if errWrite := os.WriteFile(filePath, []byte(content), 0o600); errWrite != nil {
			t.Fatalf("write auth file: %v", errWrite)
		}
	}
	writeAuthFile("pfx-0")

	configPath := filepath.Join(authDir, "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte(fmt.Sprintf("auth-dir: %q\n", filepath.ToSlash(authDir))), 0o600); errWrite != nil {
		t.Fatalf("write config file: %v", errWrite)
	}

	cfg := &config.Config{AuthDir: authDir}
	service, errBuild := NewBuilder().WithConfig(cfg).WithConfigPath(configPath).Build()
	if errBuild != nil {
		t.Fatalf("Build() failed: %v", errBuild)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	watcherWrapper, errWatcher := service.watcherFactory(configPath, authDir, func(*config.Config) {})
	if errWatcher != nil {
		t.Fatalf("watcherFactory failed: %v", errWatcher)
	}
	service.watcher = watcherWrapper
	watcherWrapper.SetConfig(service.cfg)
	service.coreManager.RegisterExecutor(&syncTestExecutor{})
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(fileName) })

	newAuth := func(prefix string) *coreauth.Auth {
		return &coreauth.Auth{
			ID:       fileName,
			FileName: fileName,
			Provider: "codex",
			Prefix:   prefix,
			Status:   coreauth.StatusActive,
			Metadata: map[string]any{
				"type":         "codex",
				"email":        "user@example.com",
				"access_token": "token",
				"prefix":       prefix,
			},
			Attributes: map[string]string{"path": filePath},
		}
	}
	if _, errRegister := service.coreManager.Register(ctx, newAuth("pfx-0")); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	hook := service.runtimeAuthSyncHook()

	// A persist that lands normally is applied once and not reapplied from disk.
	persisted, errUpdate := service.coreManager.Update(ctx, newAuth("pfx-5"))
	if errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}
	writeAuthFile("pfx-5")
	if errHook := hook(ctx, persisted); errHook != nil {
		t.Fatalf("runtime auth sync hook: %v", errHook)
	}
	if settled, _ := service.coreManager.GetByID(fileName); settled.Generation != persisted.Generation+1 {
		t.Fatalf("expected one manager update from the hook, generation %d -> %d", persisted.Generation, settled.Generation)
	}

	// PATCH 9 updates the manager and persists the file.
	persisted, errUpdate = service.coreManager.Update(ctx, newAuth("pfx-9"))
	if errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}
	writeAuthFile("pfx-9")

	// A file-event snapshot of PATCH 8's content lands before PATCH 9's hook runs.
	service.handleAuthUpdate(coreauth.WithSkipPersist(ctx), watcher.AuthUpdate{
		Action: watcher.AuthUpdateActionModify,
		ID:     fileName,
		Auth:   newAuth("pfx-8"),
	})

	if errHook := hook(ctx, persisted); errHook != nil {
		t.Fatalf("runtime auth sync hook: %v", errHook)
	}

	current, ok := service.coreManager.GetByID(fileName)
	if !ok || current == nil {
		t.Fatal("expected auth to remain registered")
	}
	if current.Prefix != "pfx-9" {
		t.Fatalf("expected persisted prefix pfx-9 after hook, got %q", current.Prefix)
	}
}

func TestPersistedMetadataApplied(t *testing.T) {
	persisted := &coreauth.Auth{Metadata: map[string]any{"prefix": "pfx-9"}}
	withHints := &coreauth.Auth{Metadata: map[string]any{"prefix": "pfx-9", antigravityPersistedHintsMetaKey: map[string]any{"x": true}}}
	if !persistedMetadataApplied(withHints, persisted) {
		t.Fatal("expected runtime-only hints to be ignored")
	}
	stale := &coreauth.Auth{Metadata: map[string]any{"prefix": "pfx-8"}}
	if persistedMetadataApplied(stale, persisted) {
		t.Fatal("expected differing file content to be reported as not applied")
	}
}
