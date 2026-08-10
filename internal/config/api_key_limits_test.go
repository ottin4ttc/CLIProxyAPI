package config

import "testing"

func TestRPMLimitForAPIKeyPrecedence(t *testing.T) {
	cfg := &SDKConfig{
		CodexBuckets: map[string]CodexBucket{
			"anon": {APIKeys: []string{"sk-anon", "sk-anon-override"}},
		},
		APIKeyLimits: APIKeyLimits{
			DefaultRPM:    40,
			ExemptBuckets: []string{"anon"},
			Overrides: map[string]int{
				"sk-heavy":         120,
				"sk-anon-override": 10,
				"sk-zero":          0,
			},
		},
	}
	cases := []struct {
		name   string
		apiKey string
		want   int
	}{
		{"unmapped key falls back to default", "sk-plain", 40},
		{"override wins over default", "sk-heavy", 120},
		{"bucket exemption yields unlimited", "sk-anon", 0},
		{"override wins over bucket exemption", "sk-anon-override", 10},
		{"explicit zero override exempts the key", "sk-zero", 0},
		{"empty key is unlimited", "", 0},
	}
	for _, tc := range cases {
		if got := cfg.RPMLimitForAPIKey(tc.apiKey); got != tc.want {
			t.Fatalf("%s: RPMLimitForAPIKey(%q) = %d, want %d", tc.name, tc.apiKey, got, tc.want)
		}
	}
	var nilCfg *SDKConfig
	if got := nilCfg.RPMLimitForAPIKey("sk-plain"); got != 0 {
		t.Fatalf("nil receiver = %d, want 0", got)
	}
}

func TestRPMLimitForAPIKeyTrimsConfiguredKey(t *testing.T) {
	cfg := &SDKConfig{APIKeyLimits: APIKeyLimits{
		DefaultRPM: 40,
		Overrides:  map[string]int{" sk-heavy ": 120},
	}}
	if got := cfg.RPMLimitForAPIKey("sk-heavy"); got != 120 {
		t.Fatalf("RPMLimitForAPIKey(sk-heavy) = %d, want 120 (configured key should be trimmed)", got)
	}
	// The caller's key is compared as-is, matching CodexBucketForAPIKey.
	if got := cfg.RPMLimitForAPIKey(" sk-heavy "); got != 40 {
		t.Fatalf("RPMLimitForAPIKey(\" sk-heavy \") = %d, want 40 (caller key is not trimmed)", got)
	}
}

func TestRPMLimitForContextValue(t *testing.T) {
	cfg := &SDKConfig{APIKeyLimits: APIKeyLimits{
		DefaultRPM: 40,
		Overrides:  map[string]int{"sk-heavy": 120},
	}}
	if got := cfg.RPMLimitForContextValue("sk-heavy"); got != 120 {
		t.Fatalf("RPMLimitForContextValue(sk-heavy) = %d, want 120", got)
	}
	if got := cfg.RPMLimitForContextValue(nil); got != 0 {
		t.Fatalf("RPMLimitForContextValue(nil) = %d, want 0", got)
	}
}

func TestValidateAPIKeyLimits(t *testing.T) {
	base := func() *SDKConfig {
		return &SDKConfig{
			CodexBuckets: map[string]CodexBucket{"anon": {APIKeys: []string{"sk-anon"}}},
			APIKeyLimits: APIKeyLimits{DefaultRPM: 40, ExemptBuckets: []string{"anon"}},
		}
	}
	if err := base().ValidateAPIKeyLimits(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	empty := &SDKConfig{}
	if err := empty.ValidateAPIKeyLimits(); err != nil {
		t.Fatalf("absent block rejected: %v", err)
	}

	negDefault := base()
	negDefault.APIKeyLimits.DefaultRPM = -1
	if err := negDefault.ValidateAPIKeyLimits(); err == nil {
		t.Fatal("expected error for negative default-rpm")
	}

	negOverride := base()
	negOverride.APIKeyLimits.Overrides = map[string]int{"sk-a": -5}
	if err := negOverride.ValidateAPIKeyLimits(); err == nil {
		t.Fatal("expected error for negative override")
	}

	unknownBucket := base()
	unknownBucket.APIKeyLimits.ExemptBuckets = []string{"anno"}
	if err := unknownBucket.ValidateAPIKeyLimits(); err == nil {
		t.Fatal("expected error for exempt-buckets naming an unknown bucket")
	}

	blankBucket := base()
	blankBucket.APIKeyLimits.ExemptBuckets = []string{"  "}
	if err := blankBucket.ValidateAPIKeyLimits(); err == nil {
		t.Fatal("expected error for whitespace-only exempt bucket name")
	}
}
