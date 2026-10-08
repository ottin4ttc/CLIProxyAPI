package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const forkRootsLegacyConfig = `port: 8317
api-keys: ["k1", "k2"]
codex-buckets:
  anon:
    api-keys: ["k2"]
api-key-limits:
  default-rpm: 60
  exempt-buckets: ["anon"]
  overrides:
    k1: 120
model-access:
  enabled: true
  rules:
    - models: ["deepseek-*"]
      api-keys: ["k1"]
codex-bucket-model-routes:
  enabled: true
  rules:
    - bucket: default
      from: gpt-5.6-sol
      provider: antigravity
      to: gemini-3.8-flash-high
credential-max-inflight:
  codex: 1
save-health-ring: true
`

func TestForkRootsDoNotMarkLegacyConfigAsV8(t *testing.T) {
	var doc yaml.Node
	if errUnmarshal := yaml.Unmarshal([]byte(forkRootsLegacyConfig), &doc); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if IsV8ConfigLayout(doc.Content[0]) {
		t.Fatal("legacy config with fork roots was detected as v8 layout")
	}
}

func TestV8MigrationKeepsForkRoots(t *testing.T) {
	migrated, _, errMigrate := NormalizeConfigLayout([]byte(forkRootsLegacyConfig), true)
	if errMigrate != nil {
		t.Fatalf("migrate: %v", errMigrate)
	}
	for _, root := range forkConfigRoots {
		if strings.Contains(string(migrated), "# "+root+":") {
			t.Fatalf("fork root %s was commented out:\n%s", root, migrated)
		}
	}
	if errValidate := ValidateV8Config(migrated); errValidate != nil {
		t.Fatalf("validate migrated config: %v", errValidate)
	}

	var cfg Config
	if errUnmarshal := yaml.Unmarshal(migrated, &cfg); errUnmarshal != nil {
		t.Fatalf("decode migrated config: %v", errUnmarshal)
	}
	if got := cfg.CodexBuckets["anon"].APIKeys; len(got) != 1 || got[0] != "k2" {
		t.Fatalf("codex-buckets.anon.api-keys = %v", got)
	}
	if cfg.APIKeyLimits.DefaultRPM != 60 || cfg.APIKeyLimits.Overrides["k1"] != 120 || len(cfg.APIKeyLimits.ExemptBuckets) != 1 {
		t.Fatalf("api-key-limits = %+v", cfg.APIKeyLimits)
	}
	if !cfg.ModelAccess.Enabled || len(cfg.ModelAccess.Rules) != 1 {
		t.Fatalf("model-access = %+v", cfg.ModelAccess)
	}
	if !cfg.CodexBucketModelRoutes.Enabled || len(cfg.CodexBucketModelRoutes.Rules) != 1 || cfg.CodexBucketModelRoutes.Rules[0].To != "gemini-3.8-flash-high" {
		t.Fatalf("codex-bucket-model-routes = %+v", cfg.CodexBucketModelRoutes)
	}
	if cfg.CredentialMaxInFlight["codex"] != 1 {
		t.Fatalf("credential-max-inflight = %v", cfg.CredentialMaxInFlight)
	}
	if !cfg.SaveHealthRing {
		t.Fatal("save-health-ring was lost")
	}
}
