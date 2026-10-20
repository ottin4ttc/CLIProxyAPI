package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Manager aggregates authenticators and coordinates persistence via a token store.
type Manager struct {
	authenticators map[string]Authenticator
	store          coreauth.Store
}

// NewManager constructs a manager with the provided token store and authenticators.
// If store is nil, the caller must set it later using SetStore.
func NewManager(store coreauth.Store, authenticators ...Authenticator) *Manager {
	mgr := &Manager{
		authenticators: make(map[string]Authenticator),
		store:          store,
	}
	for i := range authenticators {
		mgr.Register(authenticators[i])
	}
	return mgr
}

// Register adds or replaces an authenticator keyed by its provider identifier.
func (m *Manager) Register(a Authenticator) {
	if a == nil {
		return
	}
	if m.authenticators == nil {
		m.authenticators = make(map[string]Authenticator)
	}
	m.authenticators[a.Provider()] = a
}

// SetStore updates the token store used for persistence.
func (m *Manager) SetStore(store coreauth.Store) {
	m.store = store
}

// Login executes the provider login flow and persists the resulting auth record.
func (m *Manager) Login(ctx context.Context, provider string, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, string, error) {
	auth, ok := m.authenticators[provider]
	if !ok {
		return nil, "", fmt.Errorf("cliproxy auth: authenticator %s not registered", provider)
	}

	record, err := auth.Login(ctx, loginConfigWithProxy(cfg, opts), opts)
	if err != nil {
		return nil, "", err
	}
	if record == nil {
		return nil, "", fmt.Errorf("cliproxy auth: authenticator %s returned nil record", provider)
	}
	applyLoginProxyMetadata(record, opts)

	if m.store == nil {
		return record, "", nil
	}

	if cfg != nil {
		if dirSetter, ok := m.store.(interface{ SetBaseDir(string) }); ok {
			dirSetter.SetBaseDir(cfg.AuthDir)
		}
		if strings.TrimSpace(cfg.AuthDir) != "" {
			targetFile := record.FileName
			if targetFile == "" {
				targetFile = record.ID
			}
			if targetFile != "" {
				fullPath := filepath.Join(cfg.AuthDir, targetFile)
				if raw, errRead := os.ReadFile(fullPath); errRead == nil && len(raw) > 0 {
					var existingMap map[string]any
					if errUnmarshal := json.Unmarshal(raw, &existingMap); errUnmarshal == nil && len(existingMap) > 0 {
						coreauth.MergeExistingAuthMetadata(record, existingMap)
					}
				}
			}
		}
	}
	legacyClaudeCredential, errLegacy := claudeauth.FindMatchingLegacyCredential(ctx, m.store, record)
	if errLegacy != nil {
		return record, "", errLegacy
	}
	if legacyClaudeCredential != nil {
		coreauth.MergeExistingAuthMetadata(record, legacyClaudeCredential.Metadata)
	}

	savedPath, err := m.store.Save(coreauth.WithAuthCreationIntent(ctx), record)
	if err != nil {
		return record, "", err
	}
	if legacyClaudeCredential != nil {
		if strings.TrimSpace(savedPath) == "" {
			return record, "", fmt.Errorf("cliproxy auth: canonical Claude credential was not persisted; legacy credential retained")
		}
		legacyID := strings.TrimSpace(legacyClaudeCredential.ID)
		if legacyID == "" {
			legacyID = strings.TrimSpace(legacyClaudeCredential.FileName)
		}
		if errDelete := m.store.Delete(ctx, legacyID); errDelete != nil {
			return record, savedPath, fmt.Errorf("cliproxy auth: canonical Claude credential saved but legacy credential cleanup failed: %w", errDelete)
		}
	}
	return record, savedPath, nil
}

// loginConfigWithProxy returns cfg with the per-login proxy override applied.
// Authenticators read the global proxy from cfg, so the override is expressed
// as a runtime clone rather than a provider-specific option.
func loginConfigWithProxy(cfg *config.Config, opts *LoginOptions) *config.Config {
	if cfg == nil || opts == nil {
		return cfg
	}
	proxyURL := strings.TrimSpace(opts.ProxyURL)
	if proxyURL == "" || proxyURL == strings.TrimSpace(cfg.ProxyURL) {
		return cfg
	}
	cloned := cfg.CloneForRuntime()
	if cloned == nil {
		return cfg
	}
	cloned.ProxyURL = proxyURL
	return cloned
}

// applyLoginProxyMetadata persists the login proxy on the credential so the
// runtime and token refresh use the same egress.
func applyLoginProxyMetadata(record *coreauth.Auth, opts *LoginOptions) {
	if record == nil || opts == nil {
		return
	}
	proxyURL := strings.TrimSpace(opts.ProxyURL)
	if proxyURL == "" {
		return
	}
	if record.Metadata == nil {
		record.Metadata = make(map[string]any)
	}
	record.Metadata["proxy_url"] = proxyURL
	record.ProxyURL = proxyURL
	if proxyRef := strings.TrimSpace(opts.ProxyRef); proxyRef != "" {
		record.Metadata["proxy_ref"] = proxyRef
	} else {
		// An explicit proxy URL wins over a stale reference carried by the
		// existing credential; the empty value keeps the merge from restoring it.
		record.Metadata["proxy_ref"] = ""
	}
	if timezone := strings.TrimSpace(opts.Timezone); timezone != "" {
		record.Metadata["timezone"] = timezone
	}
}
