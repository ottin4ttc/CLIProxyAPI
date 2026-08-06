package api

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/convstore"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// dummyPluginHost is a minimal handlers.PluginInterceptorHost implementation
// used to verify identity pass-through when the native store is disabled.
type dummyPluginHost struct{}

func (dummyPluginHost) InterceptRequestBeforeAuth(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{}
}

func (dummyPluginHost) InterceptRequestAfterAuth(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{}
}

func (dummyPluginHost) InterceptResponse(context.Context, pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
	return pluginapi.ResponseInterceptResponse{}
}

func (dummyPluginHost) InterceptStreamChunk(context.Context, pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	return pluginapi.StreamChunkInterceptResponse{}
}

// writeConvstoreConfig writes body to a fresh temp config file and returns
// its path.
func writeConvstoreConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// enabledConvstoreConfigPath writes a config file whose top-level
// conversation-store block is enabled, backed by a fresh t.TempDir() data
// dir so the test never writes into the repo.
func enabledConvstoreConfigPath(t *testing.T) string {
	t.Helper()
	body := fmt.Sprintf(`
conversation-store:
  enabled: true
  data-dir: %q
  max-body-bytes: 1048576
`, t.TempDir())
	return writeConvstoreConfig(t, body)
}

// TestConvstoreHostLifecycle exercises convstoreHost's enable/reconfigure/disable/
// re-enable branches against the package-level convstoreState singleton. Subtests
// run sequentially (no t.Parallel) since they share and mutate that state.
func TestConvstoreHostLifecycle(t *testing.T) {
	// Start from a known-clean slate in case another test left state behind.
	convstoreHost("", nil)
	t.Cleanup(func() {
		convstoreHost("", nil)
	})

	t.Run("disabled config returns inner unchanged", func(t *testing.T) {
		disabledPath := writeConvstoreConfig(t, "log:\n  level: info\n")
		var inner handlers.PluginInterceptorHost = dummyPluginHost{}
		if got := convstoreHost(disabledPath, inner); got != inner {
			t.Fatalf("convstoreHost() = %#v, want inner unchanged", got)
		}
		if got := convstoreHost(disabledPath, nil); got != nil {
			t.Fatalf("convstoreHost() with nil inner = %#v, want nil", got)
		}
	})

	var firstStore *convstore.Store

	t.Run("enabled config creates a store and returns a hook", func(t *testing.T) {
		got := convstoreHost(enabledConvstoreConfigPath(t), dummyPluginHost{})
		if _, ok := got.(*convstore.Hook); !ok {
			t.Fatalf("convstoreHost() returned %T, want *convstore.Hook", got)
		}

		convstoreState.mu.Lock()
		firstStore = convstoreState.store
		convstoreState.mu.Unlock()
		if firstStore == nil {
			t.Fatal("convstoreState.store is nil after enabling")
		}
	})

	t.Run("calling again with enabled config reconfigures the same store", func(t *testing.T) {
		got := convstoreHost(enabledConvstoreConfigPath(t), dummyPluginHost{})
		if _, ok := got.(*convstore.Hook); !ok {
			t.Fatalf("convstoreHost() returned %T, want *convstore.Hook", got)
		}

		convstoreState.mu.Lock()
		store := convstoreState.store
		convstoreState.mu.Unlock()
		if store != firstStore {
			t.Fatalf("convstoreState.store changed on reconfigure: got %p, want %p", store, firstStore)
		}
	})

	t.Run("disabled config after enabled shuts the store down and resets state", func(t *testing.T) {
		disabledPath := writeConvstoreConfig(t, "log:\n  level: info\n")
		var inner handlers.PluginInterceptorHost = dummyPluginHost{}
		if got := convstoreHost(disabledPath, inner); got != inner {
			t.Fatalf("convstoreHost() = %#v, want inner unchanged", got)
		}

		convstoreState.mu.Lock()
		store, stop := convstoreState.store, convstoreState.stop
		convstoreState.mu.Unlock()
		if store != nil {
			t.Fatalf("convstoreState.store = %p, want nil after disable", store)
		}
		if stop != nil {
			t.Fatal("convstoreState.stop is non-nil after disable")
		}

		// Calling disable again must not panic (no double-close of stop channel).
		if got := convstoreHost(disabledPath, inner); got != inner {
			t.Fatalf("repeated disable: convstoreHost() = %#v, want inner unchanged", got)
		}
	})

	t.Run("re-enable after disable creates a new store instance", func(t *testing.T) {
		got := convstoreHost(enabledConvstoreConfigPath(t), dummyPluginHost{})
		if _, ok := got.(*convstore.Hook); !ok {
			t.Fatalf("convstoreHost() returned %T, want *convstore.Hook", got)
		}

		convstoreState.mu.Lock()
		newStore := convstoreState.store
		convstoreState.mu.Unlock()
		if newStore == nil {
			t.Fatal("convstoreState.store is nil after re-enabling")
		}
		if newStore == firstStore {
			t.Fatal("convstoreState.store was resurrected: expected a new instance after Shutdown, got the same pointer")
		}
	})
}

// TestConvstoreHostPluginKeyIgnored locks in the decoupling from the plugin
// system: a config file whose only conversation-store mention is under
// plugins.configs (the old gate) must NOT enable native recording, because
// the top-level conversation-store block is absent.
func TestConvstoreHostPluginKeyIgnored(t *testing.T) {
	convstoreHost("", nil)
	t.Cleanup(func() {
		convstoreHost("", nil)
	})

	path := writeConvstoreConfig(t, fmt.Sprintf(`
plugins:
  enabled: false
  configs:
    conversation-store:
      enabled: true
      data-dir: %q
      max-body-bytes: 1048576
`, t.TempDir()))

	var inner handlers.PluginInterceptorHost = dummyPluginHost{}
	got := convstoreHost(path, inner)
	if got != inner {
		t.Fatalf("convstoreHost() = %#v, want inner unchanged (plugin key must not gate native recording)", got)
	}

	convstoreState.mu.Lock()
	store := convstoreState.store
	convstoreState.mu.Unlock()
	if store != nil {
		t.Fatalf("convstoreState.store = %p, want nil: plugins.configs.conversation-store.enabled must not create a store", store)
	}
}

// TestConvstoreHostMissingPath covers passthrough with no panic for an empty
// path and a path that does not exist on disk.
func TestConvstoreHostMissingPath(t *testing.T) {
	convstoreHost("", nil)
	t.Cleanup(func() {
		convstoreHost("", nil)
	})

	var inner handlers.PluginInterceptorHost = dummyPluginHost{}

	if got := convstoreHost("", inner); got != inner {
		t.Fatalf("convstoreHost(\"\") = %#v, want inner unchanged", got)
	}

	nonexistent := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	if got := convstoreHost(nonexistent, inner); got != inner {
		t.Fatalf("convstoreHost(nonexistent) = %#v, want inner unchanged", got)
	}
}

// TestConvstoreShutdown proves the parity fix for process-exit flushing: once
// enabled, a recorded turn must land on disk after convstoreShutdown() (not
// just after an explicit store.Shutdown() call from a test), state must be
// nil'd out, and a second call must be a no-op rather than a double-close
// panic.
func TestConvstoreShutdown(t *testing.T) {
	// Start from a known-clean slate in case another test left state behind.
	convstoreHost("", nil)
	t.Cleanup(func() {
		convstoreHost("", nil)
	})

	dataDir := t.TempDir()
	path := writeConvstoreConfig(t, fmt.Sprintf(`
conversation-store:
  enabled: true
  data-dir: %q
  max-body-bytes: 1048576
`, dataDir))

	got := convstoreHost(path, dummyPluginHost{})
	hook, ok := got.(*convstore.Hook)
	if !ok {
		t.Fatalf("convstoreHost() returned %T, want *convstore.Hook", got)
	}

	ctx := context.Background()
	hook.InterceptRequestBeforeAuth(ctx, pluginapi.RequestInterceptRequest{
		RequestID: "shutdown-1",
		Model:     "m",
		Headers:   map[string][]string{"Authorization": {"Bearer test-key"}},
		Body:      []byte(`{"model":"m"}`),
	})
	hook.InterceptResponse(ctx, pluginapi.ResponseInterceptRequest{
		RequestID: "shutdown-1",
		Body:      []byte(`{"ok":true}`),
	})

	convstoreShutdown()

	convstoreState.mu.Lock()
	store, stop := convstoreState.store, convstoreState.stop
	convstoreState.mu.Unlock()
	if store != nil {
		t.Fatalf("convstoreState.store = %p, want nil after shutdown", store)
	}
	if stop != nil {
		t.Fatal("convstoreState.stop is non-nil after shutdown")
	}

	// The recorded turn must have made it to disk: convstoreShutdown() blocks
	// on the writer draining its queue, so this file must exist by now.
	var found []string
	errWalk := filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, errWalk error) error {
		if errWalk != nil {
			return errWalk
		}
		if !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
			found = append(found, path)
		}
		return nil
	})
	if errWalk != nil {
		t.Fatalf("walk data dir: %v", errWalk)
	}
	if len(found) != 1 {
		t.Fatalf("found %d jsonl files under %s, want 1: %v", len(found), dataDir, found)
	}
	data, errRead := os.ReadFile(found[0])
	if errRead != nil {
		t.Fatalf("read jsonl: %v", errRead)
	}
	if !strings.Contains(string(data), `"status":"ok"`) {
		t.Fatalf("recorded line missing status ok: %s", data)
	}

	// Second call must be a no-op, not a double-close panic.
	convstoreShutdown()
}
