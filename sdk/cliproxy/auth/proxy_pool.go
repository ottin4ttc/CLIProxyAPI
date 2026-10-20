package auth

import (
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// ProxyRefMetadataKey is the credential metadata key naming a proxy-pool entry.
// When a credential carries it, the pool's current URL overrides the persisted
// proxy_url at load, registration, config reload, and selection time.
const ProxyRefMetadataKey = "proxy_ref"

// applyProxyPoolRef rewrites auth.ProxyURL from its proxy_ref using cfg's pool.
// The persisted proxy_url stays in place as the fallback when the reference is
// missing or no longer configured.
func applyProxyPoolRef(auth *Auth, cfg *internalconfig.Config) {
	if auth == nil || cfg == nil {
		return
	}
	ref := authMetadataString(auth, ProxyRefMetadataKey)
	if ref == "" {
		return
	}
	if resolved, ok := cfg.ProxyPool.Resolve(ref); ok {
		auth.ProxyURL = resolved
		return
	}
	if persisted := authMetadataString(auth, "proxy_url"); persisted != "" {
		auth.ProxyURL = persisted
	}
}

// applyProxyPoolRef resolves one auth against the manager's current config.
func (m *Manager) applyProxyPoolRef(auth *Auth) {
	if m == nil {
		return
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	applyProxyPoolRef(auth, cfg)
}

// applyProxyPoolRefsLocked re-resolves every registered auth after the config
// snapshot changed. Scheduler snapshots stay stale, but selection resolves the
// returned copy again, so request-time egress is always current.
func (m *Manager) applyProxyPoolRefsLocked(cfg *internalconfig.Config) {
	if m == nil || cfg == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, auth := range m.auths {
		applyProxyPoolRef(auth, cfg)
	}
}
