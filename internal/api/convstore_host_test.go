package api

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
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

// enabledConvstoreConfig builds a config.Config whose plugins.configs.conversation-store
// block is populated the same way production YAML parsing populates it (via
// PluginInstanceConfig.UnmarshalYAML, which preserves the raw subtree convstoreHost
// re-marshals for convstore.ParseConfig). data-dir is always a fresh t.TempDir() so
// the test never writes into the repo. plugins.enabled is deliberately left false to
// lock in the "global plugin loader switch is ignored" contract.
func enabledConvstoreConfig(t *testing.T) *config.Config {
	t.Helper()
	src := fmt.Sprintf(`
plugins:
  enabled: false
  configs:
    conversation-store:
      enabled: true
      data-dir: %q
      max-body-bytes: 1048576
`, t.TempDir())

	var cfg config.Config
	if errUnmarshal := yaml.Unmarshal([]byte(src), &cfg); errUnmarshal != nil {
		t.Fatalf("unmarshal config: %v", errUnmarshal)
	}
	return &cfg
}

// TestConvstoreHostLifecycle exercises convstoreHost's enable/reconfigure/disable/
// re-enable branches against the package-level convstoreState singleton. Subtests
// run sequentially (no t.Parallel) since they share and mutate that state.
func TestConvstoreHostLifecycle(t *testing.T) {
	// Start from a known-clean slate in case another test left state behind.
	convstoreHost(&config.Config{}, nil)
	t.Cleanup(func() {
		convstoreHost(&config.Config{}, nil)
	})

	t.Run("disabled config returns inner unchanged", func(t *testing.T) {
		var inner handlers.PluginInterceptorHost = dummyPluginHost{}
		if got := convstoreHost(&config.Config{}, inner); got != inner {
			t.Fatalf("convstoreHost() = %#v, want inner unchanged", got)
		}
		if got := convstoreHost(&config.Config{}, nil); got != nil {
			t.Fatalf("convstoreHost() with nil inner = %#v, want nil", got)
		}
	})

	var firstStore *convstore.Store

	t.Run("enabled config creates a store and returns a hook", func(t *testing.T) {
		got := convstoreHost(enabledConvstoreConfig(t), dummyPluginHost{})
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
		got := convstoreHost(enabledConvstoreConfig(t), dummyPluginHost{})
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
		var inner handlers.PluginInterceptorHost = dummyPluginHost{}
		if got := convstoreHost(&config.Config{}, inner); got != inner {
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
		if got := convstoreHost(&config.Config{}, inner); got != inner {
			t.Fatalf("repeated disable: convstoreHost() = %#v, want inner unchanged", got)
		}
	})

	t.Run("re-enable after disable creates a new store instance", func(t *testing.T) {
		got := convstoreHost(enabledConvstoreConfig(t), dummyPluginHost{})
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

// TestConvstoreShutdown proves the parity fix for process-exit flushing: once
// enabled, a recorded turn must land on disk after convstoreShutdown() (not
// just after an explicit store.Shutdown() call from a test), state must be
// nil'd out, and a second call must be a no-op rather than a double-close
// panic.
func TestConvstoreShutdown(t *testing.T) {
	// Start from a known-clean slate in case another test left state behind.
	convstoreHost(&config.Config{}, nil)
	t.Cleanup(func() {
		convstoreHost(&config.Config{}, nil)
	})

	dataDir := t.TempDir()
	src := fmt.Sprintf(`
plugins:
  enabled: false
  configs:
    conversation-store:
      enabled: true
      data-dir: %q
      max-body-bytes: 1048576
`, dataDir)
	var cfg config.Config
	if errUnmarshal := yaml.Unmarshal([]byte(src), &cfg); errUnmarshal != nil {
		t.Fatalf("unmarshal config: %v", errUnmarshal)
	}

	got := convstoreHost(&cfg, dummyPluginHost{})
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
