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
// lower model tier when the upstream reports the model as overloaded. It is the
// single gate for both the rotation cap and the fallback itself, so a request
// it rejects keeps exactly the behaviour it had before this feature existed.
// Both halves must agree: capping rotation without allowing the degrade would
// leave the caller worse off than either behaviour on its own.
func (m *Manager) codexFallbackEligible(providers []string, model string, opts cliproxyexecutor.Options) bool {
	if m == nil || !hasCodexProvider(providers) {
		return false
	}
	// Home mode ignores maxRetryCredentials in execute*MixedOnce, so the
	// one-credential-per-tier budget this feature relies on would be void:
	// each tier could burn through every Home credential during an overload
	// incident, multiplied by the chain length. Decline rather than degrade,
	// and therefore keep the unbounded rotation Home mode had before.
	if m.HomeEnabled() {
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
	// codexFallbackEligible carries the Home-mode gate, so the rotation cap and
	// the degrade share one source of truth.
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

// The requested-model metadata is deliberately left untouched while walking the
// chain. The response already reports the serving tier: the codex executor
// stamps req.Model into the translated response, and fallbackReq.Model is the
// tier. The auth layer's only response relabeling, rewriteForceMappedResponse
// and the wrapStreamResult rewriter, is gated on OAuthModelAliasResult
// .ForceMapping, whose OriginalAlias comes from the configured OAuth alias (or,
// under Home mode, from auth attributes) - never from this metadata key. Keeping
// the key at the client's original model preserves requested_model !=
// resolved_model in usage_events as the fallback-rate query.

// codexFallbackLogEntry describes one step of a chain walk. The auth field is
// read back from the metadata the executor publishes for the credential it
// selected (publishSelectedAuthMetadata); it is omitted when the caller supplied
// no metadata map for that to be published into.
//
// The field is the last credential this request touched, not necessarily the
// credential that ran the tier this log line reports on: publishSelectedAuthMetadata
// only runs once a credential is actually selected, so a tier that fails during
// credential selection itself (no credential available for that tier, the
// pickNextMixed path) leaves the map holding whatever an earlier tier - or the
// primary sweep before any fallback - last published. Treat "auth" as a hint
// about recent activity on this request, not an attribution of which tier failed.
func codexFallbackLogEntry(ctx context.Context, opts cliproxyexecutor.Options, model, tier string) *log.Entry {
	fields := log.Fields{"from": model, "to": tier, "reason": FailureCauseOverload}
	if authID := stringMetadataValue(opts.Metadata, cliproxyexecutor.SelectedAuthMetadataKey); authID != "" {
		fields["auth"] = authID
	}
	return logEntryWithRequestID(ctx).WithFields(fields)
}

// codexFallbackExhaustedLogEntry describes a chain walk in which no tier served
// the request, which is the case an operator most needs to see.
func codexFallbackExhaustedLogEntry(ctx context.Context, model string, chain []string) *log.Entry {
	return logEntryWithRequestID(ctx).WithFields(log.Fields{
		"from":   model,
		"chain":  strings.Join(chain, ","),
		"reason": FailureCauseOverload,
	})
}

// tryCodexModelFallback walks the configured chain once, trying a single
// credential per tier. ok is false when no tier succeeded.
func (m *Manager) tryCodexModelFallback(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, model string) (cliproxyexecutor.Response, bool, error) {
	chain := m.codexFallbackChain(model)
	for _, tier := range chain {
		if ctx.Err() != nil {
			return cliproxyexecutor.Response{}, false, nil
		}
		fallbackReq := req
		fallbackReq.Model = tier
		resp, errExec := m.executeMixedOnce(withCodexModelFallback(ctx), providers, fallbackReq, opts, 1)
		if errExec == nil {
			codexFallbackLogEntry(ctx, opts, model, tier).Warn("codex model fallback served the request")
			return resp, true, nil
		}
		codexFallbackLogEntry(ctx, opts, model, tier).Warnf("codex model fallback tier failed: %v", errExec)
	}
	if len(chain) > 0 {
		codexFallbackExhaustedLogEntry(ctx, model, chain).Warn("codex model fallback exhausted the chain, returning the original error")
	}
	return cliproxyexecutor.Response{}, false, nil
}

// tryCodexModelFallbackCount is the count-tokens counterpart of
// tryCodexModelFallback. It stays on the count executor so a degraded tier
// still returns a token count rather than a completion payload.
func (m *Manager) tryCodexModelFallbackCount(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, model string) (cliproxyexecutor.Response, bool, error) {
	chain := m.codexFallbackChain(model)
	for _, tier := range chain {
		if ctx.Err() != nil {
			return cliproxyexecutor.Response{}, false, nil
		}
		fallbackReq := req
		fallbackReq.Model = tier
		resp, errExec := m.executeCountMixedOnce(withCodexModelFallback(ctx), providers, fallbackReq, opts, 1)
		if errExec == nil {
			codexFallbackLogEntry(ctx, opts, model, tier).Warn("codex model fallback served the count_tokens request")
			return resp, true, nil
		}
		codexFallbackLogEntry(ctx, opts, model, tier).Warnf("codex model fallback tier failed for count_tokens: %v", errExec)
	}
	if len(chain) > 0 {
		codexFallbackExhaustedLogEntry(ctx, model, chain).Warn("codex model fallback exhausted the chain for count_tokens, returning the original error")
	}
	return cliproxyexecutor.Response{}, false, nil
}

// tryCodexModelFallbackStream is the streaming counterpart of
// tryCodexModelFallback. It only runs before any bytes reach the client.
func (m *Manager) tryCodexModelFallbackStream(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, model string) (*cliproxyexecutor.StreamResult, bool, error) {
	chain := m.codexFallbackChain(model)
	for _, tier := range chain {
		if ctx.Err() != nil {
			return nil, false, nil
		}
		fallbackReq := req
		fallbackReq.Model = tier
		result, errStream := m.executeStreamMixedOnce(withCodexModelFallback(ctx), providers, fallbackReq, opts, 1)
		if errStream == nil {
			codexFallbackLogEntry(ctx, opts, model, tier).Warn("codex model fallback served the stream")
			return result, true, nil
		}
		codexFallbackLogEntry(ctx, opts, model, tier).Warnf("codex model fallback tier failed for the stream: %v", errStream)
	}
	if len(chain) > 0 {
		codexFallbackExhaustedLogEntry(ctx, model, chain).Warn("codex model fallback exhausted the chain for the stream, returning the original error")
	}
	return nil, false, nil
}
