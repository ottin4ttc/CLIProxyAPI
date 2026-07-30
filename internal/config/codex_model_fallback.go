package config

import (
	"fmt"
	"strings"
)

// CodexFallbackChain returns the ordered fallback tiers configured for model,
// or nil when the model has no chain. Lookup trims whitespace and ignores case.
func (c *Config) CodexFallbackChain(model string) []string {
	if c == nil {
		return nil
	}
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return nil
	}
	for _, entry := range c.Codex.ModelFallback {
		if strings.ToLower(strings.TrimSpace(entry.From)) != key {
			continue
		}
		chain := make([]string, 0, len(entry.To))
		for _, tier := range entry.To {
			if trimmed := strings.TrimSpace(tier); trimmed != "" {
				chain = append(chain, trimmed)
			}
		}
		return chain
	}
	return nil
}

// ValidateCodexModelFallback rejects chains that are blank, empty, duplicated
// or self-referencing.
func (c *Config) ValidateCodexModelFallback() error {
	if c == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(c.Codex.ModelFallback))
	for _, entry := range c.Codex.ModelFallback {
		from := strings.TrimSpace(entry.From)
		if from == "" {
			return fmt.Errorf("codex.model-fallback: 'from' must not be empty or whitespace")
		}
		key := strings.ToLower(from)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("codex.model-fallback: model %q is mapped more than once", from)
		}
		seen[key] = struct{}{}

		tiers := 0
		for _, tier := range entry.To {
			trimmed := strings.TrimSpace(tier)
			if trimmed == "" {
				continue
			}
			if strings.EqualFold(trimmed, from) {
				return fmt.Errorf("codex.model-fallback: model %q must not fall back to itself", from)
			}
			tiers++
		}
		if tiers == 0 {
			return fmt.Errorf("codex.model-fallback: model %q needs at least one non-empty fallback tier", from)
		}
	}
	return nil
}
