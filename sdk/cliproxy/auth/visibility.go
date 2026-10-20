package auth

import (
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// FilterVisibleModels reports which of the given models at least one credential
// visible to the bucket can serve. excludes maps a model ID to provider-access
// selectors the requesting client key may not use.
//
// Visibility follows the same bucket and provider-access rules as credential
// selection so a model only appears when the caller could actually use it.
// Operator-disabled credentials are ignored; temporary cooldowns and quota
// state do not affect visibility, keeping listings stable during brief
// upstream failures.
func (m *Manager) FilterVisibleModels(models []string, bucket string, excludes map[string][]string) map[string]bool {
	visible := make(map[string]bool, len(models))
	if m == nil || len(models) == 0 {
		return visible
	}
	auths := m.snapshotAuths()
	if len(auths) == 0 {
		for _, model := range models {
			if model = strings.TrimSpace(model); model != "" {
				visible[model] = true
			}
		}
		return visible
	}
	bucketIdx := m.loadBucketIndex()
	registryRef := registry.GetGlobalRegistry()
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		providers := registryRef.GetModelProviders(model)
		if len(providers) == 0 {
			// No provider mapping to check against: keep the model visible
			// instead of hiding a model the registry cannot explain.
			visible[model] = true
			continue
		}
		eligibility := authSelectionEligibility{
			bucket:           bucket,
			buckets:          bucketIdx,
			excludeProviders: normalizeExcludeProviders(excludes[model]),
		}
		for _, auth := range auths {
			if auth == nil || auth.Disabled {
				continue
			}
			if !authServesProvider(auth, providers) {
				continue
			}
			if !eligibility.allows(auth) {
				continue
			}
			visible[model] = true
			break
		}
	}
	return visible
}

func authServesProvider(auth *Auth, providers []string) bool {
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "" {
		return false
	}
	for _, candidate := range providers {
		if strings.ToLower(strings.TrimSpace(candidate)) == provider {
			return true
		}
	}
	return false
}

// UnbucketedOAuthAuthIDs returns enabled OAuth credentials of provider that
// carry no bucket tag. Such credentials are visible to client keys that are
// not mapped to any bucket, so deployments that partition OAuth accounts by
// bucket need to tag every account.
func (m *Manager) UnbucketedOAuthAuthIDs(provider string) []string {
	if m == nil {
		return nil
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	var ids []string
	for _, candidate := range m.snapshotAuths() {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if strings.ToLower(strings.TrimSpace(candidate.Provider)) != provider {
			continue
		}
		if candidate.AuthKind() != AuthKindOAuth || authBucket(candidate) != "" {
			continue
		}
		ids = append(ids, candidate.ID)
	}
	sort.Strings(ids)
	return ids
}
