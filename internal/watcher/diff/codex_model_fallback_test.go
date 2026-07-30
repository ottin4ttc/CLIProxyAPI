package diff

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestDiffCodexModelFallbackChanges_Added(t *testing.T) {
	newChains := []config.CodexModelFallback{
		{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra", "gpt-5.6-luna"}},
	}

	changes := DiffCodexModelFallbackChanges(nil, newChains)
	expectContains(t, changes, "codex.model-fallback[gpt-5.6-sol]: <none> -> gpt-5.6-terra,gpt-5.6-luna")
}

func TestDiffCodexModelFallbackChanges_Removed(t *testing.T) {
	oldChains := []config.CodexModelFallback{
		{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra", "gpt-5.6-luna"}},
	}

	changes := DiffCodexModelFallbackChanges(oldChains, nil)
	expectContains(t, changes, "codex.model-fallback[gpt-5.6-sol]: gpt-5.6-terra,gpt-5.6-luna -> <none>")
}

func TestDiffCodexModelFallbackChanges_ToListChanged(t *testing.T) {
	oldChains := []config.CodexModelFallback{
		{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}},
	}
	newChains := []config.CodexModelFallback{
		{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra", "gpt-5.6-luna"}},
	}

	changes := DiffCodexModelFallbackChanges(oldChains, newChains)
	expectContains(t, changes, "codex.model-fallback[gpt-5.6-sol]: gpt-5.6-terra -> gpt-5.6-terra,gpt-5.6-luna")
}

func TestDiffCodexModelFallbackChanges_Reordered(t *testing.T) {
	oldChains := []config.CodexModelFallback{
		{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra", "gpt-5.6-luna"}},
	}
	newChains := []config.CodexModelFallback{
		{From: "gpt-5.6-sol", To: []string{"gpt-5.6-luna", "gpt-5.6-terra"}},
	}

	changes := DiffCodexModelFallbackChanges(oldChains, newChains)
	expectContains(t, changes, "codex.model-fallback[gpt-5.6-sol]: reordered (gpt-5.6-terra,gpt-5.6-luna -> gpt-5.6-luna,gpt-5.6-terra)")
}

func TestDiffCodexModelFallbackChanges_NoChange(t *testing.T) {
	chains := []config.CodexModelFallback{
		{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra", "gpt-5.6-luna"}},
	}

	changes := DiffCodexModelFallbackChanges(chains, chains)
	if len(changes) != 0 {
		t.Fatalf("expected no change entries, got %v", changes)
	}
}

func TestDiffCodexModelFallbackChanges_NoChangeEmpty(t *testing.T) {
	changes := DiffCodexModelFallbackChanges(nil, nil)
	if len(changes) != 0 {
		t.Fatalf("expected no change entries for empty chains, got %v", changes)
	}
}

func TestBuildConfigChangeDetails_CodexModelFallback(t *testing.T) {
	oldCfg := &config.Config{}
	newCfg := &config.Config{
		Codex: config.CodexConfig{
			ModelFallback: []config.CodexModelFallback{
				{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra", "gpt-5.6-luna"}},
			},
		},
	}

	changes := BuildConfigChangeDetails(oldCfg, newCfg)
	expectContains(t, changes, "codex.model-fallback[gpt-5.6-sol]: <none> -> gpt-5.6-terra,gpt-5.6-luna")
}
