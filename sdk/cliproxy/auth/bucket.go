package auth

import "strings"

// isBucketScopedProvider reports whether credentials of the provider are
// partitioned by buckets. Other providers ignore bucket tags.
func isBucketScopedProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex", "claude":
		return true
	default:
		return false
	}
}

// isBucketScopedAuth reports whether bucket tags partition the credential.
// Only OAuth accounts of bucket-scoped providers are partitioned; API-key
// credentials (e.g. claude-api-key upstreams) are shared by every client key.
func isBucketScopedAuth(auth *Auth) bool {
	return auth != nil && isBucketScopedProvider(auth.Provider) && auth.AuthKind() != AuthKindAPIKey
}

// authBucket returns the credential's bucket tag, or "" when unbucketed.
// Attributes take precedence over raw auth-file metadata, mirroring authWeight.
func authBucket(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if raw, ok := auth.Attributes[AttributeBucket]; ok {
		if bucket := strings.TrimSpace(raw); bucket != "" {
			return bucket
		}
	}
	if raw, ok := auth.Metadata[AttributeBucket]; ok {
		if value, okString := raw.(string); okString {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// bucketIndex maps each bucket-scoped provider to the bucket names carried by
// its OAuth credentials. Disabled credentials count, so disabling a bucket's
// last account does not move its keys into the shared pool.
type bucketIndex map[string]map[string]struct{}

func buildBucketIndex(auths map[string]*Auth) bucketIndex {
	idx := make(bucketIndex)
	for _, auth := range auths {
		if !isBucketScopedAuth(auth) {
			continue
		}
		bucket := authBucket(auth)
		if bucket == "" {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if idx[provider] == nil {
			idx[provider] = make(map[string]struct{})
		}
		idx[provider][bucket] = struct{}{}
	}
	return idx
}

// has reports whether any OAuth credential of provider is tagged with bucket.
func (idx bucketIndex) has(provider, bucket string) bool {
	_, ok := idx[strings.ToLower(strings.TrimSpace(provider))][bucket]
	return ok
}

// rebuildBucketIndexLocked recomputes the bucket index from m.auths.
// Callers must hold m.mu for writing.
func (m *Manager) rebuildBucketIndexLocked() {
	idx := buildBucketIndex(m.auths)
	m.bucketIndex.Store(&idx)
}

// loadBucketIndex returns the current bucket index, or nil before any auth
// has been registered.
func (m *Manager) loadBucketIndex() bucketIndex {
	if m == nil {
		return nil
	}
	if idx := m.bucketIndex.Load(); idx != nil {
		return *idx
	}
	return nil
}
