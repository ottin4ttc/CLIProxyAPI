package api

import (
	"os"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/convstore"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
)

// convstoreState owns the native conversation store lifecycle across server
// construction and config hot reloads.
var convstoreState struct {
	mu    sync.Mutex
	store *convstore.Store
	stop  chan struct{}
}

// convstoreHookFor wraps inner with the current store. Callers hold
// convstoreState.mu.
func convstoreHookFor(inner handlers.PluginInterceptorHost) handlers.PluginInterceptorHost {
	return convstore.NewHook(convstoreState.store, inner)
}

// convstoreFileConfig extracts the dedicated top-level conversation-store
// block from the raw config file. The block is invisible to config.Config
// (unknown top-level keys are ignored there), which keeps the recorder fully
// decoupled from the plugin system.
type convstoreFileConfig struct {
	ConversationStore yaml.Node `yaml:"conversation-store"`
}

// convstoreDisableLocked shuts down a running store, if any, and resets
// state. Callers must hold convstoreState.mu. No-op when no store is
// running.
func convstoreDisableLocked() {
	if convstoreState.store == nil {
		return
	}
	close(convstoreState.stop)
	convstoreState.store.Shutdown()
	convstoreState.store = nil
	convstoreState.stop = nil
	log.Info("convstore: native conversation recording disabled")
}

// convstoreHost wraps the plugin interceptor host with native conversation
// recording when the top-level conversation-store block in the config file
// has enabled: true. This is intentionally decoupled from the plugin system:
// the block is read directly from configFilePath rather than from
// config.Config, so plugins.configs.conversation-store (and the global
// plugins.enabled dylib loader switch) have no bearing on it. Called from
// both initial server construction and config hot reload; safe to call
// repeatedly.
func convstoreHost(configFilePath string, inner handlers.PluginInterceptorHost) handlers.PluginInterceptorHost {
	convstoreState.mu.Lock()
	defer convstoreState.mu.Unlock()

	if configFilePath == "" {
		convstoreDisableLocked()
		return inner
	}

	data, errRead := os.ReadFile(configFilePath)
	if errRead != nil {
		log.Errorf("convstore: read config file: %v", errRead)
		convstoreDisableLocked()
		return inner
	}

	var fileCfg convstoreFileConfig
	if errUnmarshal := yaml.Unmarshal(data, &fileCfg); errUnmarshal != nil {
		log.Errorf("convstore: parse config file: %v", errUnmarshal)
		convstoreDisableLocked()
		return inner
	}

	if fileCfg.ConversationStore.IsZero() {
		convstoreDisableLocked()
		return inner
	}

	var gate struct {
		Enabled bool `yaml:"enabled"`
	}
	if errDecode := fileCfg.ConversationStore.Decode(&gate); errDecode != nil {
		log.Errorf("convstore: decode conversation-store block: %v", errDecode)
		convstoreDisableLocked()
		return inner
	}
	if !gate.Enabled {
		convstoreDisableLocked()
		return inner
	}

	raw, errMarshal := yaml.Marshal(&fileCfg.ConversationStore)
	if errMarshal != nil {
		log.Errorf("convstore: marshal config block: %v", errMarshal)
		convstoreDisableLocked()
		return inner
	}
	storeCfg, errParse := convstore.ParseConfig(raw)
	if errParse != nil {
		log.Errorf("convstore: parse config: %v", errParse)
		convstoreDisableLocked()
		return inner
	}

	if convstoreState.store == nil {
		convstoreState.store = convstore.New(storeCfg, time.Now)
		convstoreState.stop = make(chan struct{})
		go func(st *convstore.Store, stop chan struct{}) {
			defer func() {
				if r := recover(); r != nil {
					log.Errorf("convstore: maintenance loop panic: %v", r)
				}
			}()
			st.StartMaintenance(stop)
		}(convstoreState.store, convstoreState.stop)
		log.Info("convstore: native conversation recording enabled")
	} else {
		convstoreState.store.Reconfigure(storeCfg)
	}
	return convstoreHookFor(inner)
}

// convstoreShutdown flushes and stops the native conversation store, if
// one is running. Called from Server.Stop so queued JSONL appends drain
// before process exit.
func convstoreShutdown() {
	convstoreState.mu.Lock()
	defer convstoreState.mu.Unlock()
	if convstoreState.store == nil {
		return
	}
	close(convstoreState.stop)
	convstoreState.store.Shutdown()
	convstoreState.store = nil
	convstoreState.stop = nil
	log.Info("convstore: native conversation recording stopped")
}
