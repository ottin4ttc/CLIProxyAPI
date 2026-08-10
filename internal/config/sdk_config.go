// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import (
	"fmt"
	"strings"
)

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

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

	// CodexOptimizeMultiAgentV2 mirrors the provider-wide runtime setting for API handlers.
	CodexOptimizeMultiAgentV2 bool `yaml:"-" json:"-"`

	// ClaudeCode configures Claude Code compatibility behavior.
	ClaudeCode ClaudeCodeConfig `yaml:"claude-code" json:"claude-code"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// CodexBuckets maps bucket names to the client API keys assigned to them.
	// A mapped key only uses codex credentials whose auth file carries the same
	// top-level "bucket" value; unmapped keys only use unbucketed credentials.
	CodexBuckets map[string]CodexBucket `yaml:"codex-buckets" json:"codex-buckets"`

	// APIKeyLimits configures per-client-API-key request rate limits.
	APIKeyLimits APIKeyLimits `yaml:"api-key-limits" json:"api-key-limits"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`
}

// ClaudeCodeConfig configures Claude Code compatibility behavior.
type ClaudeCodeConfig struct {
	// DisableCloakingModelList disables model ID cloaking in Anthropic model list responses.
	DisableCloakingModelList bool `yaml:"disable-cloaking-model-list" json:"disable-cloaking-model-list"`
}

// CodexBucket groups client API keys allowed to use codex credentials tagged
// with the bucket's name.
type CodexBucket struct {
	// APIKeys lists the client API keys mapped to this bucket.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`
}

// CodexBucketForAPIKey returns the bucket name the client API key is mapped
// to, or the empty string when the key is unmapped. Configured keys are
// trimmed before comparison (matching ValidateCodexBuckets); apiKey is
// compared as-is since it comes directly from the caller's request.
func (c *SDKConfig) CodexBucketForAPIKey(apiKey string) string {
	if c == nil || apiKey == "" {
		return ""
	}
	for name, bucket := range c.CodexBuckets {
		for _, key := range bucket.APIKeys {
			if strings.TrimSpace(key) == apiKey {
				return name
			}
		}
	}
	return ""
}

// CodexBucketForContextValue resolves the codex bucket for a raw context
// value (typically a gin "userApiKey" context entry) by formatting it and
// delegating to CodexBucketForAPIKey. It exists so every call site that
// reads the client API key out of request context (handlers, codex-only
// side channels) shares one lookup implementation instead of re-deriving
// it. Returns "" when v is nil or the key is unmapped.
func (c *SDKConfig) CodexBucketForContextValue(v any) string {
	if c == nil || v == nil {
		return ""
	}
	return c.CodexBucketForAPIKey(fmt.Sprint(v))
}

// ValidateCodexBuckets rejects configurations that map one client API key
// into more than one bucket.
func (c *SDKConfig) ValidateCodexBuckets() error {
	if c == nil {
		return nil
	}
	seen := make(map[string]string)
	for name, bucket := range c.CodexBuckets {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("codex-buckets: bucket name must not be empty or whitespace")
		}
		for _, key := range bucket.APIKeys {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			if prev, ok := seen[key]; ok && prev != name {
				return fmt.Errorf("codex-buckets: an api key is mapped to both bucket %q and bucket %q", prev, name)
			}
			seen[key] = name
		}
	}
	return nil
}

// APIKeyLimits configures per-client-API-key request rate limits.
type APIKeyLimits struct {
	// DefaultRPM is the requests-per-minute cap applied to every client API key
	// that is neither overridden nor bucket-exempt. 0 means unlimited.
	DefaultRPM int `yaml:"default-rpm" json:"default-rpm"`

	// ExemptBuckets lists codex bucket names whose client API keys are exempt
	// from rate limiting. Names must exist in CodexBuckets.
	ExemptBuckets []string `yaml:"exempt-buckets,omitempty" json:"exempt-buckets,omitempty"`

	// Overrides maps a client API key to its own cap. An override wins over
	// both ExemptBuckets and DefaultRPM; 0 exempts that key.
	Overrides map[string]int `yaml:"overrides,omitempty" json:"overrides,omitempty"`
}

// RPMLimitForAPIKey returns the requests-per-minute cap for a client API key.
// 0 means unlimited. Precedence is Overrides > ExemptBuckets > DefaultRPM.
// Configured keys and bucket names are trimmed before comparison, matching
// CodexBucketForAPIKey; apiKey is compared as-is since it comes straight from
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
	if bucket := c.CodexBucketForAPIKey(apiKey); bucket != "" {
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
// mirroring CodexBucketForContextValue so every call site shares one lookup.
func (c *SDKConfig) RPMLimitForContextValue(v any) int {
	if c == nil || v == nil {
		return 0
	}
	return c.RPMLimitForAPIKey(fmt.Sprint(v))
}

// ValidateAPIKeyLimits rejects negative caps, exempt-bucket names that do
// not exist in CodexBuckets, and Overrides keys that collide after trimming,
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
	// ValidateCodexBuckets rejects one api key mapped into two buckets.
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
		if _, ok := c.CodexBuckets[trimmed]; !ok {
			return fmt.Errorf("api-key-limits: exempt-buckets references unknown bucket %q", trimmed)
		}
	}
	return nil
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n").
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`
}
