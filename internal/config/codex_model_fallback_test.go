package config

import "testing"

func TestCodexFallbackChain(t *testing.T) {
	cfg := &Config{}
	cfg.Codex.ModelFallback = []CodexModelFallback{
		{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra", "gpt-5.6-luna"}},
	}

	chain := cfg.CodexFallbackChain("gpt-5.6-sol")
	if len(chain) != 2 || chain[0] != "gpt-5.6-terra" || chain[1] != "gpt-5.6-luna" {
		t.Fatalf("chain = %v, want [gpt-5.6-terra gpt-5.6-luna]", chain)
	}
	if got := cfg.CodexFallbackChain("  GPT-5.6-SOL  "); len(got) != 2 {
		t.Fatalf("chain lookup must trim and ignore case, got %v", got)
	}
	if got := cfg.CodexFallbackChain("gpt-5.6-terra"); len(got) != 0 {
		t.Fatalf("unmapped model chain = %v, want empty", got)
	}
	if got := (&Config{}).CodexFallbackChain("gpt-5.6-sol"); len(got) != 0 {
		t.Fatalf("empty config chain = %v, want empty", got)
	}
}

func TestValidateCodexModelFallback(t *testing.T) {
	valid := &Config{}
	valid.Codex.ModelFallback = []CodexModelFallback{
		{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}},
	}
	if err := valid.ValidateCodexModelFallback(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	blankFrom := &Config{}
	blankFrom.Codex.ModelFallback = []CodexModelFallback{{From: "  ", To: []string{"gpt-5.6-terra"}}}
	if err := blankFrom.ValidateCodexModelFallback(); err == nil {
		t.Fatal("blank from must be rejected")
	}

	emptyTo := &Config{}
	emptyTo.Codex.ModelFallback = []CodexModelFallback{{From: "gpt-5.6-sol"}}
	if err := emptyTo.ValidateCodexModelFallback(); err == nil {
		t.Fatal("empty to must be rejected")
	}

	selfRef := &Config{}
	selfRef.Codex.ModelFallback = []CodexModelFallback{
		{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra", "gpt-5.6-sol"}},
	}
	if err := selfRef.ValidateCodexModelFallback(); err == nil {
		t.Fatal("self-referencing chain must be rejected")
	}

	duplicate := &Config{}
	duplicate.Codex.ModelFallback = []CodexModelFallback{
		{From: "gpt-5.6-sol", To: []string{"gpt-5.6-terra"}},
		{From: "gpt-5.6-sol", To: []string{"gpt-5.6-luna"}},
	}
	if err := duplicate.ValidateCodexModelFallback(); err == nil {
		t.Fatal("duplicate from must be rejected")
	}
}
