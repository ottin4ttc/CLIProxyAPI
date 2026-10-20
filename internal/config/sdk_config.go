// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// MaxRequestBodyBytes limits the size (in bytes) of incoming HTTP request bodies.
	// Values <= 0 disable the limit (default).
	MaxRequestBodyBytes int64 `yaml:"max-request-body-bytes" json:"max-request-body-bytes"`

	// Client configures client-facing compatibility behavior.
	Client ClientConfig `yaml:"client" json:"client"`

	// OAuthOnlyFields records v8 provider settings that must wait for credential
	// selection and must not affect API-key credentials. Config YAML snapshots
	// preserve the corresponding v8 paths instead of serializing this metadata.
	OAuthOnlyFields map[string]bool `yaml:"-" json:"-"`

	// CodexResponseSteering mirrors the provider-wide runtime setting for API handlers.
	CodexResponseSteering bool `yaml:"-" json:"-"`

	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// TLSHandshakeTimeoutSeconds bounds upstream TLS handshakes made by the
	// shared uTLS client, in seconds. It applies to both the Claude Code
	// Anthropic transport and the Chrome ChatGPT/Codex transport; the
	// standard fallback transport is unaffected. It starts after TCP or
	// proxy connection establishment returns and does not bound dialing or
	// proxy CONNECT. Those uTLS paths use custom TLS dialing, for which Go
	// ignores http.Transport.TLSHandshakeTimeout, so a peer that accepts the
	// connection but never answers the ClientHello would otherwise block the
	// request indefinitely and request-retry could never engage. The bound
	// applies only during the TLS handshake and is removed once it succeeds,
	// so inference and long-running streams are unaffected. The default 0
	// disables the bound (previous behavior); negative values are treated as
	// 0, and values beyond the representable duration range saturate.
	TLSHandshakeTimeoutSeconds int `yaml:"tls-handshake-timeout-seconds" json:"tls-handshake-timeout-seconds"`

	// DisableImageGeneration controls whether the built-in image_generation tool is injected/allowed.
	//
	// Supported values:
	//   - false (default): image_generation is enabled everywhere (normal behavior).
	//   - true: image_generation is disabled everywhere. The server stops injecting it, removes it from request payloads,
	//     and returns 404 for /v1/images/generations and /v1/images/edits.
	//   - "chat": disable image_generation injection for all non-images endpoints (e.g. /v1/responses, /v1/chat/completions),
	//     while keeping /v1/images/generations and /v1/images/edits enabled and preserving image_generation there.
	//   - "passthrough": do not modify the tool list on non-images endpoints — keep image_generation if the client
	//     sent it and do not inject it otherwise; on /v1/images/generations and /v1/images/edits behave like "chat".
	DisableImageGeneration DisableImageGenerationMode `yaml:"disable-image-generation" json:"disable-image-generation"`

	// GPTImage2BaseModel sets the base (mainline) model used by the legacy hosted
	// image_generation tool path when a Codex image request is not proxied directly
	// through the Image API.
	//
	// The value must start with "gpt-" (case-insensitive). If empty or invalid, the
	// default base model ("gpt-5.4-mini") is used.
	GPTImage2BaseModel string `yaml:"gpt-image-2-base-model,omitempty" json:"gpt-image-2-base-model,omitempty"`

	// VideoResultAuthCacheTTL controls how long video IDs stay pinned to the credential
	// that created them. Accepts duration strings like "30m" or "3h".
	// Empty or invalid values use the default 3h.
	VideoResultAuthCacheTTL string `yaml:"video-result-auth-cache-ttl,omitempty" json:"video-result-auth-cache-ttl,omitempty"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// CodexOrphanDelegationCompatibility mirrors the provider-wide runtime setting for API handlers.
	CodexOrphanDelegationCompatibility bool `yaml:"-" json:"-"`

	// ClaudeCode configures Claude Code compatibility behavior.
	ClaudeCode ClaudeCodeConfig `yaml:"claude-code" json:"claude-code"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// Buckets maps bucket names to the client API keys assigned to them.
	// A mapped key only uses bucket-scoped (codex, claude) credentials whose
	// auth file carries the same top-level "bucket" value; unmapped keys only
	// use unbucketed credentials.
	Buckets map[string]Bucket `yaml:"buckets" json:"buckets"`

	// APIKeyLimits configures per-client-API-key request rate limits.
	APIKeyLimits APIKeyLimits `yaml:"api-key-limits" json:"api-key-limits"`

	// ModelAccess restricts models matching a rule to that rule's client API keys.
	ModelAccess ModelAccess `yaml:"model-access" json:"model-access"`

	// ProviderAccess excludes upstream credential channels for matched models
	// and client API keys, and hides models no visible channel can serve.
	ProviderAccess ProviderAccess `yaml:"provider-access" json:"provider-access"`

	// WebTools configures the Search/Fetch API gateway exposed under /v1/tools.
	WebTools WebTools `yaml:"web-tools" json:"web-tools"`

	// ProxyPool maps operator-defined names to upstream proxies. Credentials
	// that carry a proxy_ref resolve their runtime proxy from this pool, and
	// --login-proxy accepts a pool name.
	ProxyPool ProxyPool `yaml:"proxy-pool" json:"proxy-pool"`

	// BucketModelRoutes rewrites a requested model to another provider's
	// model for client API keys in a given bucket.
	BucketModelRoutes BucketModelRoutes `yaml:"bucket-model-routes" json:"bucket-model-routes"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`
}

// TLSHandshakeTimeout returns the configured upstream TLS handshake bound
// for requests using the shared uTLS client. A non-positive configured
// value disables the bound and yields 0.
func (c *SDKConfig) TLSHandshakeTimeout() time.Duration {
	if c == nil || c.TLSHandshakeTimeoutSeconds <= 0 {
		return 0
	}
	const maxTLSHandshakeTimeoutSeconds = math.MaxInt64 / int64(time.Second)
	if int64(c.TLSHandshakeTimeoutSeconds) > maxTLSHandshakeTimeoutSeconds {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(c.TLSHandshakeTimeoutSeconds) * time.Second
}

// ClientConfig configures client-facing compatibility behavior.
type ClientConfig struct {
	Codex CodexClientConfig `yaml:"codex" json:"codex"`
}

// CodexClientConfig configures Codex client compatibility and the model catalog.
type CodexClientConfig struct {
	// OptimizeMultiAgentV2 optimizes official Codex multi-agent requests across providers.
	// Default false leaves the client's multi-agent behavior unchanged.
	OptimizeMultiAgentV2 bool `yaml:"optimize-multi-agent-v2" json:"optimize-multi-agent-v2"`

	// EnableApplyPatch advertises freeform apply_patch only for supported models.
	// Default false clears the capability regardless of template metadata.
	EnableApplyPatch bool `yaml:"enable-apply-patch" json:"enable-apply-patch"`
}

// ClaudeCodeConfig configures Claude Code compatibility behavior.
type ClaudeCodeConfig struct {
	// DisableCloakingModelList disables model ID cloaking in Anthropic model list responses.
	DisableCloakingModelList bool `yaml:"disable-cloaking-model-list" json:"disable-cloaking-model-list"`
}

// Bucket groups client API keys allowed to use bucket-scoped credentials
// tagged with the bucket's name.
type Bucket struct {
	// APIKeys lists the client API keys mapped to this bucket.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`
}

// BucketForAPIKey returns the bucket name the client API key is mapped
// to, or the empty string when the key is unmapped. Configured keys are
// trimmed before comparison (matching ValidateBuckets); apiKey is
// compared as-is since it comes directly from the caller's request.
func (c *SDKConfig) BucketForAPIKey(apiKey string) string {
	if c == nil || apiKey == "" {
		return ""
	}
	for name, bucket := range c.Buckets {
		for _, key := range bucket.APIKeys {
			if strings.TrimSpace(key) == apiKey {
				return name
			}
		}
	}
	return ""
}

// BucketForContextValue resolves the bucket for a raw context
// value (typically a gin "userApiKey" context entry) by formatting it and
// delegating to BucketForAPIKey. It exists so every call site that
// reads the client API key out of request context (handlers, codex-only
// side channels) shares one lookup implementation instead of re-deriving
// it. Returns "" when v is nil or the key is unmapped.
func (c *SDKConfig) BucketForContextValue(v any) string {
	if c == nil || v == nil {
		return ""
	}
	return c.BucketForAPIKey(fmt.Sprint(v))
}

// ValidateBuckets rejects configurations that map one client API key
// into more than one bucket.
func (c *SDKConfig) ValidateBuckets() error {
	if c == nil {
		return nil
	}
	seen := make(map[string]string)
	for name, bucket := range c.Buckets {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("buckets: bucket name must not be empty or whitespace")
		}
		for _, key := range bucket.APIKeys {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			if prev, ok := seen[key]; ok && prev != name {
				return fmt.Errorf("buckets: an api key is mapped to both bucket %q and bucket %q", prev, name)
			}
			seen[key] = name
		}
	}
	return nil
}

// BucketModelRoutes rewrites a requested model to another provider's
// model for client API keys in a given bucket, before credential
// selection. Keys in other buckets keep the normal model resolution.
type BucketModelRoutes struct {
	// Enabled turns routing on. When false, Rules are ignored entirely.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Rules lists the per-bucket model rewrites.
	Rules []BucketModelRoute `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// BucketModelRoute sends requests for From from keys in Bucket to
// provider Provider using model To.
type BucketModelRoute struct {
	// Bucket is the bucket name; "default" or empty means keys not
	// mapped to any bucket.
	Bucket string `yaml:"bucket" json:"bucket"`

	// From is the client-requested model name (exact match).
	From string `yaml:"from" json:"from"`

	// Provider is the target provider key (e.g. antigravity, or an
	// openai-compatibility entry name).
	Provider string `yaml:"provider" json:"provider"`

	// To is the model name sent to Provider.
	To string `yaml:"to" json:"to"`
}

// DefaultBucketName is the rule bucket name that stands for keys not
// mapped to any bucket (whose bucket resolves to "").
const DefaultBucketName = "default"

func normalizeBucketRuleName(bucket string) string {
	bucket = strings.TrimSpace(bucket)
	if bucket == DefaultBucketName {
		return ""
	}
	return bucket
}

// BucketModelRoute returns the target provider and model for a request
// from a key in bucket ("" for unmapped keys) asking for model. ok is false
// when routing is disabled or no rule matches.
func (c *SDKConfig) BucketModelRoute(bucket, model string) (provider, to string, ok bool) {
	if c == nil || !c.BucketModelRoutes.Enabled {
		return "", "", false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", false
	}
	for _, rule := range c.BucketModelRoutes.Rules {
		if normalizeBucketRuleName(rule.Bucket) != bucket || strings.TrimSpace(rule.From) != model {
			continue
		}
		provider = strings.ToLower(strings.TrimSpace(rule.Provider))
		to = strings.TrimSpace(rule.To)
		if provider == "" || to == "" {
			continue
		}
		return provider, to, true
	}
	return "", "", false
}

// ValidateBucketModelRoutes rejects rules missing from/provider/to and
// rules that repeat the same bucket+from pair.
func (c *SDKConfig) ValidateBucketModelRoutes() error {
	if c == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(c.BucketModelRoutes.Rules))
	for i, rule := range c.BucketModelRoutes.Rules {
		from := strings.TrimSpace(rule.From)
		if from == "" {
			return fmt.Errorf("bucket-model-routes: rule %d has an empty from", i)
		}
		if strings.TrimSpace(rule.Provider) == "" {
			return fmt.Errorf("bucket-model-routes: rule %d (%s) has an empty provider", i, from)
		}
		if strings.TrimSpace(rule.To) == "" {
			return fmt.Errorf("bucket-model-routes: rule %d (%s) has an empty to", i, from)
		}
		key := normalizeBucketRuleName(rule.Bucket) + "\x00" + from
		if _, dup := seen[key]; dup {
			return fmt.Errorf("bucket-model-routes: model %q is routed twice for bucket %q", from, rule.Bucket)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// APIKeyLimits configures per-client-API-key request rate limits.
type APIKeyLimits struct {
	// DefaultRPM is the requests-per-minute cap applied to every client API key
	// that is neither overridden nor bucket-exempt. 0 means unlimited.
	DefaultRPM int `yaml:"default-rpm" json:"default-rpm"`

	// ExemptBuckets lists bucket names whose client API keys are exempt
	// from rate limiting. Names must exist in Buckets.
	ExemptBuckets []string `yaml:"exempt-buckets,omitempty" json:"exempt-buckets,omitempty"`

	// Overrides maps a client API key to its own cap. An override wins over
	// both ExemptBuckets and DefaultRPM; 0 exempts that key.
	Overrides map[string]int `yaml:"overrides,omitempty" json:"overrides,omitempty"`
}

// RPMLimitForAPIKey returns the requests-per-minute cap for a client API key.
// 0 means unlimited. Precedence is Overrides > ExemptBuckets > DefaultRPM.
// Configured keys and bucket names are trimmed before comparison, matching
// BucketForAPIKey; apiKey is compared as-is since it comes straight from
// the caller's request.
func (c *SDKConfig) RPMLimitForAPIKey(apiKey string) int {
	if c == nil || apiKey == "" {
		return 0
	}
	for key, limit := range c.APIKeyLimits.Overrides {
		if strings.TrimSpace(key) == apiKey {
			if limit < 0 {
				return 0
			}
			return limit
		}
	}
	if bucket := c.BucketForAPIKey(apiKey); bucket != "" {
		for _, name := range c.APIKeyLimits.ExemptBuckets {
			if strings.TrimSpace(name) == bucket {
				return 0
			}
		}
	}
	if c.APIKeyLimits.DefaultRPM < 0 {
		return 0
	}
	return c.APIKeyLimits.DefaultRPM
}

// RPMLimitForContextValue resolves the cap for a raw context value (typically a
// gin "userApiKey" entry) by formatting it and delegating to RPMLimitForAPIKey,
// mirroring BucketForContextValue so every call site shares one lookup.
func (c *SDKConfig) RPMLimitForContextValue(v any) int {
	if c == nil || v == nil {
		return 0
	}
	return c.RPMLimitForAPIKey(fmt.Sprint(v))
}

// ValidateAPIKeyLimits rejects negative caps, exempt-bucket names that do
// not exist in Buckets, and Overrides keys that collide after trimming,
// so a typo cannot silently drop an exemption and a stray space cannot leave
// two entries racing over the same key.
func (c *SDKConfig) ValidateAPIKeyLimits() error {
	if c == nil {
		return nil
	}
	if c.APIKeyLimits.DefaultRPM < 0 {
		return fmt.Errorf("api-key-limits: default-rpm must not be negative")
	}
	// RPMLimitForAPIKey compares strings.TrimSpace(key) against the caller's
	// key, so "sk-a" and " sk-a " are distinct map keys that both match the
	// same caller — Go's random map iteration order would then make the
	// effective limit flip between the two on every request. Mirrors how
	// ValidateBuckets rejects one api key mapped into two buckets.
	seenOverrideKeys := make(map[string]string)
	for key, limit := range c.APIKeyLimits.Overrides {
		if limit < 0 {
			return fmt.Errorf("api-key-limits: overrides[%q] must not be negative", key)
		}
		trimmed := strings.TrimSpace(key)
		if prev, ok := seenOverrideKeys[trimmed]; ok {
			return fmt.Errorf("api-key-limits: overrides keys %q and %q both trim to %q", prev, key, trimmed)
		}
		seenOverrideKeys[trimmed] = key
	}
	for _, name := range c.APIKeyLimits.ExemptBuckets {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return fmt.Errorf("api-key-limits: exempt-buckets must not contain an empty name")
		}
		if _, ok := c.Buckets[trimmed]; !ok {
			return fmt.Errorf("api-key-limits: exempt-buckets references unknown bucket %q", trimmed)
		}
	}
	return nil
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n")
	// or WebSocket Ping control frames.
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`

	// SendKeepAliveBeforeFT controls whether SSE heartbeats are sent before the first token arrives
	// when KeepAliveSeconds > 0. Default is false.
	SendKeepAliveBeforeFT bool `yaml:"send-keepalive-before-ft,omitempty" json:"send-keepalive-before-ft,omitempty"`
}

// ProviderAccess excludes upstream credential channels for matched models and
// client API keys. Channels are identified by credential kind ("oauth" or
// "apikey"), provider name, or "provider:kind"; an asterisk matches any
// substring. When enabled, it also hides models that no visible channel of the
// requesting key can serve.
type ProviderAccess struct {
	// Enabled turns enforcement on. When false, Rules are ignored entirely.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Rules lists model patterns, client API keys, and the channels those keys
	// may not use for the matched models.
	Rules []ProviderAccessRule `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// ProviderAccessRule excludes channels for the matched (model, client API key)
// pairs. Matching follows ModelAccessRule: model patterns accept '*', API key
// patterns accept '*' as well.
type ProviderAccessRule struct {
	// Models lists model names or wildcard patterns ('*' matches any substring).
	Models []string `yaml:"models" json:"models"`

	// APIKeys lists client API keys or wildcard patterns the rule applies to.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// ExcludeProviders lists channel selectors the matched keys may not use.
	ExcludeProviders []string `yaml:"exclude-providers,omitempty" json:"exclude-providers,omitempty"`
}

// ProviderAccessExcludeProviders returns the channel selectors the client API
// key may not use for model. Matching rules are unioned; a key and model that
// match no rule are unrestricted. A thinking suffix on model is ignored so
// "claude-sonnet-5-5(high)" matches rules written for "claude-sonnet-5-5".
func (c *SDKConfig) ProviderAccessExcludeProviders(apiKey, model string) []string {
	if c == nil || !c.ProviderAccess.Enabled || len(c.ProviderAccess.Rules) == 0 {
		return nil
	}
	model = ruleModelName(model)
	bare := model
	if idx := strings.Index(model, "/"); idx >= 0 {
		bare = model[idx+1:]
	}
	var out []string
	seen := make(map[string]struct{})
	for _, rule := range c.ProviderAccess.Rules {
		if !rule.matchesModel(model) && !rule.matchesModel(bare) {
			continue
		}
		if !rule.matchesAPIKey(apiKey) {
			continue
		}
		for _, selector := range rule.ExcludeProviders {
			selector = strings.ToLower(strings.TrimSpace(selector))
			if selector == "" {
				continue
			}
			if _, exists := seen[selector]; exists {
				continue
			}
			seen[selector] = struct{}{}
			out = append(out, selector)
		}
	}
	return out
}

func (r ProviderAccessRule) matchesModel(model string) bool {
	for _, pattern := range r.Models {
		if matchModelAccessPattern(strings.TrimSpace(pattern), model) {
			return true
		}
	}
	return false
}

func (r ProviderAccessRule) matchesAPIKey(apiKey string) bool {
	for _, pattern := range r.APIKeys {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if pattern == apiKey || matchModelAccessPattern(pattern, apiKey) {
			return true
		}
	}
	return false
}

// ruleModelName strips a thinking suffix such as "claude-sonnet-5-5(high)".
func ruleModelName(model string) string {
	model = strings.TrimSpace(model)
	if idx := strings.LastIndex(model, "("); idx > 0 && strings.HasSuffix(model, ")") {
		return model[:idx]
	}
	return model
}

// ValidateProviderAccess rejects rules that cannot be enforced.
func (c *SDKConfig) ValidateProviderAccess() error {
	if c == nil || !c.ProviderAccess.Enabled {
		return nil
	}
	for i, rule := range c.ProviderAccess.Rules {
		if len(rule.Models) == 0 {
			return fmt.Errorf("provider-access: rule %d: models must not be empty", i)
		}
		if len(rule.APIKeys) == 0 {
			return fmt.Errorf("provider-access: rule %d: api-keys must not be empty", i)
		}
		for _, selector := range rule.ExcludeProviders {
			selector = strings.ToLower(strings.TrimSpace(selector))
			if selector == "" {
				return fmt.Errorf("provider-access: rule %d: exclude-providers entries must not be empty", i)
			}
			name, kind, hasKind := strings.Cut(selector, ":")
			if name == "" {
				return fmt.Errorf("provider-access: rule %d: invalid provider selector %q", i, selector)
			}
			if hasKind && kind != "oauth" && kind != "apikey" {
				return fmt.Errorf("provider-access: rule %d: invalid provider selector %q", i, selector)
			}
		}
	}
	return nil
}

// ModelAccess restricts models matching a rule to that rule's client API keys.
type ModelAccess struct {
	// Enabled turns enforcement on. When false, Rules are ignored entirely.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Rules lists model patterns and the client API keys allowed to use them.
	Rules []ModelAccessRule `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// WebTools configures the Search/Fetch API gateway exposed under /v1/tools.
// The gateway forwards to a DeepSeek-compatible Search/Fetch API
// (POST {base-url}/search and POST {base-url}/fetch) so client tooling can
// share one upstream credential instead of holding its own key.
type WebTools struct {
	// Enabled exposes the gateway. When false the routes answer 404, so a
	// deployment that does not need the gateway is indistinguishable from one
	// that never registered it.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// BaseURL is the upstream base URL including its path prefix; "/search"
	// and "/fetch" are appended. Defaults to the DeepSeek Search/Fetch API.
	BaseURL string `yaml:"base-url,omitempty" json:"base-url,omitempty"`

	// APIKey is a literal upstream API key. When empty, Credential selects an
	// openai-compatibility entry whose api-key-entries supply the key.
	APIKey string `yaml:"api-key,omitempty" json:"api-key,omitempty"`

	// Credential names an openai-compatibility entry to borrow the upstream
	// key from. The first api-key-entry with a usable key is used.
	Credential string `yaml:"credential,omitempty" json:"credential,omitempty"`

	// Count is the result count applied when the client omits "count".
	// Values outside 1..50 are clamped.
	Count int `yaml:"count,omitempty" json:"count,omitempty"`

	// NeedContent applies need_content=true when the client omits it, so
	// results carry page bodies instead of summaries only.
	NeedContent bool `yaml:"need-content,omitempty" json:"need-content,omitempty"`
}

// ModelAccessRule allows only APIKeys to use models matching any of Models.
type ModelAccessRule struct {
	// Models lists model names or wildcard patterns ('*' matches any substring).
	Models []string `yaml:"models" json:"models"`

	// APIKeys lists the client API keys allowed to use the matched models. An
	// empty list locks the matched models for every key.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`
}

// ModelAccessAllowed reports whether the client API key may use model. Models
// matched by no rule are always allowed; a matched model is allowed only when
// at least one matching rule lists the key. A "prefix/model" request is also
// matched by its bare model name so provider prefixes need no separate rule.
// Configured keys are trimmed before comparison, matching BucketForAPIKey;
// apiKey is compared as-is since it comes straight from the caller's request.
func (c *SDKConfig) ModelAccessAllowed(apiKey, model string) bool {
	if c == nil || !c.ModelAccess.Enabled {
		return true
	}
	model = strings.TrimSpace(model)
	bare := model
	if idx := strings.Index(model, "/"); idx >= 0 {
		bare = model[idx+1:]
	}
	matched := false
	for _, rule := range c.ModelAccess.Rules {
		if !rule.matches(model) && !rule.matches(bare) {
			continue
		}
		matched = true
		for _, key := range rule.APIKeys {
			if key = strings.TrimSpace(key); key != "" && key == apiKey {
				return true
			}
		}
	}
	return !matched
}

func (r ModelAccessRule) matches(model string) bool {
	for _, pattern := range r.Models {
		if matchModelAccessPattern(strings.TrimSpace(pattern), model) {
			return true
		}
	}
	return false
}

// matchModelAccessPattern matches value against pattern where '*' matches any
// substring, including the empty one. An empty pattern matches nothing.
func matchModelAccessPattern(pattern, value string) bool {
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
