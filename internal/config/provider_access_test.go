package config

import "testing"

func TestProviderAccessExcludeProvidersUnionsMatchingRules(t *testing.T) {
	cfg := &SDKConfig{ProviderAccess: ProviderAccess{
		Enabled: true,
		Rules: []ProviderAccessRule{
			{
				Models:           []string{"claude-*"},
				APIKeys:          []string{"sk-sub-*"},
				ExcludeProviders: []string{"apikey"},
			},
			{
				Models:           []string{"claude-sonnet-5-5"},
				APIKeys:          []string{"sk-sub-a"},
				ExcludeProviders: []string{"claude:apikey", "apikey"},
			},
		},
	}}

	got := cfg.ProviderAccessExcludeProviders("sk-sub-a", "claude-sonnet-5-5")
	if len(got) != 2 || got[0] != "apikey" || got[1] != "claude:apikey" {
		t.Fatalf("ProviderAccessExcludeProviders() = %#v, want [apikey claude:apikey]", got)
	}
	if got := cfg.ProviderAccessExcludeProviders("sk-other", "claude-sonnet-5-5"); len(got) != 0 {
		t.Fatalf("unmatched key got %#v, want no exclusions", got)
	}
	if got := cfg.ProviderAccessExcludeProviders("sk-sub-a", "gpt-5.5"); len(got) != 0 {
		t.Fatalf("unmatched model got %#v, want no exclusions", got)
	}
}

func TestProviderAccessExcludeProvidersIgnoresThinkingSuffix(t *testing.T) {
	cfg := &SDKConfig{ProviderAccess: ProviderAccess{
		Enabled: true,
		Rules: []ProviderAccessRule{{
			Models:           []string{"claude-sonnet-5-5"},
			APIKeys:          []string{"sk-sub"},
			ExcludeProviders: []string{"apikey"},
		}},
	}}
	got := cfg.ProviderAccessExcludeProviders("sk-sub", "claude-sonnet-5-5(high)")
	if len(got) != 1 || got[0] != "apikey" {
		t.Fatalf("ProviderAccessExcludeProviders() = %#v, want [apikey]", got)
	}
}

func TestProviderAccessDisabled(t *testing.T) {
	cfg := &SDKConfig{ProviderAccess: ProviderAccess{
		Enabled: false,
		Rules: []ProviderAccessRule{{
			Models:           []string{"claude-*"},
			APIKeys:          []string{"*"},
			ExcludeProviders: []string{"apikey"},
		}},
	}}
	if got := cfg.ProviderAccessExcludeProviders("sk-any", "claude-sonnet-5-5"); len(got) != 0 {
		t.Fatalf("disabled provider access returned %#v, want no exclusions", got)
	}
	if err := cfg.ValidateProviderAccess(); err != nil {
		t.Fatalf("ValidateProviderAccess() = %v, want nil", err)
	}
}

func TestValidateProviderAccess(t *testing.T) {
	valid := &SDKConfig{ProviderAccess: ProviderAccess{
		Enabled: true,
		Rules: []ProviderAccessRule{{
			Models:           []string{"claude-*"},
			APIKeys:          []string{"sk-*"},
			ExcludeProviders: []string{"apikey", "claude:oauth", "openai-compatible-*"},
		}},
	}}
	if err := valid.ValidateProviderAccess(); err != nil {
		t.Fatalf("ValidateProviderAccess() = %v, want nil", err)
	}

	invalid := []ProviderAccessRule{
		{APIKeys: []string{"sk-*"}, ExcludeProviders: []string{"apikey"}},
		{Models: []string{"claude-*"}, ExcludeProviders: []string{"apikey"}},
		{Models: []string{"claude-*"}, APIKeys: []string{"sk-*"}, ExcludeProviders: []string{"claude:unknown"}},
		{Models: []string{"claude-*"}, APIKeys: []string{"sk-*"}, ExcludeProviders: []string{":oauth"}},
	}
	for i, rule := range invalid {
		cfg := &SDKConfig{ProviderAccess: ProviderAccess{Enabled: true, Rules: []ProviderAccessRule{rule}}}
		if err := cfg.ValidateProviderAccess(); err == nil {
			t.Fatalf("case %d: ValidateProviderAccess() = nil, want error", i)
		}
	}
}
