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
