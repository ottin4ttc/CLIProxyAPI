package auth

import (
	"context"
	"errors"
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
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
// Responses protocol or came from an official Codex client. This covers both
// the standard "/v1/responses" route group and the "/backend-api/codex"
// route group, which is the chatgpt_base_url-compatible alias a real Codex
// CLI hits when pointed at this proxy (see internal/api/server_routes.go).
// Those clients retry upstream overloads themselves and carry conversation
// state across turns, so this proxy must neither cap their credential
// rotation nor rewrite the model they asked for.
func codexNativeRequest(opts cliproxyexecutor.Options) bool {
	if originator := strings.TrimSpace(opts.Headers.Get("Originator")); originator != "" {
		return true
	}
	path := requestPathFromOptions(opts)
	return strings.HasPrefix(path, "/v1/responses") || strings.HasPrefix(path, "/backend-api/codex")
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

// codexModelFallbackKey marks a request that is already running on a fallback
// tier, so a failing tier never triggers another round of fallback.
type codexModelFallbackKey struct{}

func withCodexModelFallback(ctx context.Context) context.Context {
	return context.WithValue(ctx, codexModelFallbackKey{}, true)
}

func codexModelFallbackActive(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	active, _ := ctx.Value(codexModelFallbackKey{}).(bool)
	return active
}

// shouldAttemptCodexModelFallback reports whether lastErr represents a
// model-wide upstream overload that a lower tier could still serve.
func (m *Manager) shouldAttemptCodexModelFallback(ctx context.Context, lastErr error, providers []string, model string, opts cliproxyexecutor.Options) bool {
	if m == nil || lastErr == nil || codexModelFallbackActive(ctx) {
		return false
	}
	if isRequestTerminatedError(lastErr) || isRequestInvalidError(lastErr) {
		return false
	}
	if !m.codexFallbackEligible(providers, model, opts) {
		return false
	}
	if failureCauseFromError(lastErr) == FailureCauseOverload {
		return true
	}
	var cooldownErr *modelCooldownError
	if errors.As(lastErr, &cooldownErr) && cooldownErr != nil {
		return cooldownErr.reason == FailureCauseOverload
	}
	return false
}

// tryCodexModelFallback walks the configured chain once, trying a single
// credential per tier. ok is false when no tier succeeded.
func (m *Manager) tryCodexModelFallback(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, model string) (cliproxyexecutor.Response, bool, error) {
	for _, tier := range m.codexFallbackChain(model) {
		if ctx.Err() != nil {
			return cliproxyexecutor.Response{}, false, nil
		}
		fallbackReq := req
		fallbackReq.Model = tier
		resp, errExec := m.executeMixedOnce(withCodexModelFallback(ctx), providers, fallbackReq, opts, 1)
		if errExec == nil {
			log.WithFields(log.Fields{"from": model, "to": tier, "reason": FailureCauseOverload}).Warn("codex model fallback served the request")
			return resp, true, nil
		}
	}
	return cliproxyexecutor.Response{}, false, nil
}

// tryCodexModelFallbackStream is the streaming counterpart of
// tryCodexModelFallback. It only runs before any bytes reach the client.
func (m *Manager) tryCodexModelFallbackStream(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, model string) (*cliproxyexecutor.StreamResult, bool, error) {
	for _, tier := range m.codexFallbackChain(model) {
		if ctx.Err() != nil {
			return nil, false, nil
		}
		fallbackReq := req
		fallbackReq.Model = tier
		result, errStream := m.executeStreamMixedOnce(withCodexModelFallback(ctx), providers, fallbackReq, opts, 1)
		if errStream == nil {
			log.WithFields(log.Fields{"from": model, "to": tier, "reason": FailureCauseOverload}).Warn("codex model fallback served the stream")
			return result, true, nil
		}
	}
	return nil, false, nil
}
