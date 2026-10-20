package auth

import "strings"

// authMatchesProviderSelector reports whether a provider-access selector
// matches the credential. Selectors are one of:
//   - "oauth" or "apikey": matches the credential kind
//   - "<provider>": matches the provider name
//   - "<provider>:<kind>": matches both
//
// An asterisk inside the provider name matches any substring.
func authMatchesProviderSelector(selector string, auth *Auth) bool {
	if auth == nil {
		return false
	}
	selector = strings.ToLower(strings.TrimSpace(selector))
	if selector == "" {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	kind := strings.ToLower(strings.TrimSpace(auth.AuthKind()))
	if name, wantKind, hasKind := strings.Cut(selector, ":"); hasKind {
		if wantKind != "" && wantKind != kind {
			return false
		}
		return matchProviderPattern(name, provider)
	}
	switch selector {
	case AuthKindOAuth, AuthKindAPIKey:
		return selector == kind
	}
	return matchProviderPattern(selector, provider)
}

// authExcludedByProviderSelectors reports whether any selector excludes auth.
func authExcludedByProviderSelectors(auth *Auth, selectors []string) bool {
	for _, selector := range selectors {
		if authMatchesProviderSelector(selector, auth) {
			return true
		}
	}
	return false
}

// matchProviderPattern matches value against pattern where '*' matches any
// substring, including the empty one. An empty pattern matches nothing.
func matchProviderPattern(pattern, value string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	if prefix := parts[0]; !strings.HasPrefix(value, prefix) {
		return false
	} else {
		value = value[len(prefix):]
	}
	if suffix := parts[len(parts)-1]; !strings.HasSuffix(value, suffix) {
		return false
	} else {
		value = value[:len(value)-len(suffix)]
	}
	for _, part := range parts[1 : len(parts)-1] {
		if part == "" {
			continue
		}
		idx := strings.Index(value, part)
		if idx < 0 {
			return false
		}
		value = value[idx+len(part):]
	}
	return true
}
