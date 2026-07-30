package auth

import (
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// requestPathFromOptions returns the inbound request path recorded in options
// metadata, or an empty string when it is absent.
func requestPathFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.RequestPathMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

// codexNativeRequest reports whether the inbound request speaks Codex's own
// Responses protocol or came from an official Codex client. Those clients
// retry upstream overloads themselves and carry conversation state across
// turns, so this proxy must neither cap their credential rotation nor rewrite
// the model they asked for.
func codexNativeRequest(opts cliproxyexecutor.Options) bool {
	if originator := strings.TrimSpace(opts.Headers.Get("Originator")); originator != "" {
		return true
	}
	return strings.HasPrefix(requestPathFromOptions(opts), "/v1/responses")
}

// hasCodexProvider reports whether codex is among the candidate providers.
func hasCodexProvider(providers []string) bool {
	for _, provider := range providers {
		if strings.EqualFold(strings.TrimSpace(provider), "codex") {
			return true
		}
	}
	return false
}

// codexFallbackChain resolves the configured fallback tiers for model.
func (m *Manager) codexFallbackChain(model string) []string {
	if m == nil {
		return nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return cfg.CodexFallbackChain(model)
}

// codexFallbackEligible reports whether this request may be degraded to a
// lower model tier when the upstream reports the model as overloaded. It gates
// both the rotation cap and the fallback itself, so a request it rejects keeps
// exactly the behaviour it had before this feature existed.
func (m *Manager) codexFallbackEligible(providers []string, model string, opts cliproxyexecutor.Options) bool {
	if m == nil || !hasCodexProvider(providers) {
		return false
	}
	if codexNativeRequest(opts) {
		return false
	}
	return len(m.codexFallbackChain(model)) > 0
}
