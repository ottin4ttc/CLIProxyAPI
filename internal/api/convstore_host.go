package api

import (
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
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

// convstoreHost wraps the plugin interceptor host with native conversation
// recording when plugins.configs.conversation-store.enabled is true. The
// global plugins.enabled switch (dylib loader) is intentionally ignored so
// the dylib subsystem can stay disabled. Called from both initial server
// construction and config hot reload; safe to call repeatedly.
func convstoreHost(cfg *config.Config, inner handlers.PluginInterceptorHost) handlers.PluginInterceptorHost {
	convstoreState.mu.Lock()
	defer convstoreState.mu.Unlock()

	inst, ok := cfg.Plugins.Configs["conversation-store"]
	enabled := ok && inst.Enabled != nil && *inst.Enabled
	if !enabled {
		if convstoreState.store != nil {
			close(convstoreState.stop)
			convstoreState.store.Shutdown()
			convstoreState.store = nil
			convstoreState.stop = nil
			log.Info("convstore: native conversation recording disabled")
		}
		return inner
	}

	raw, errMarshal := yaml.Marshal(&inst.Raw)
	if errMarshal != nil {
		log.Errorf("convstore: marshal config block: %v", errMarshal)
		return inner
	}
	storeCfg, errParse := convstore.ParseConfig(raw)
	if errParse != nil {
		log.Errorf("convstore: parse config: %v", errParse)
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
