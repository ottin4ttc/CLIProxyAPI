package config

import (
	"os"
	"path/filepath"
	"testing"
)

func bucketModelRoutesTestConfig(enabled bool) *SDKConfig {
	return &SDKConfig{CodexBucketModelRoutes: CodexBucketModelRoutes{
		Enabled: enabled,
		Rules: []CodexBucketModelRoute{
			{Bucket: "default", From: "gpt-5.6-sol", Provider: "antigravity", To: "gemini-3.8-flash-high"},
			{Bucket: "", From: "gpt-5.6-terra", Provider: " Antigravity ", To: " gemini-3.8-flash-high "},
			{Bucket: "team-a", From: "gpt-5.6-sol", Provider: "deepseek", To: "deepseek-chat"},
		},
	}}
}

func TestCodexBucketModelRouteDisabledIgnoresRules(t *testing.T) {
	cfg := bucketModelRoutesTestConfig(false)
	if _, _, ok := cfg.CodexBucketModelRoute("", "gpt-5.6-sol"); ok {
		t.Fatal("disabled routes must not match")
	}
	var nilCfg *SDKConfig
	if _, _, ok := nilCfg.CodexBucketModelRoute("", "gpt-5.6-sol"); ok {
		t.Fatal("nil receiver must not match")
	}
}

func TestCodexBucketModelRouteEnabled(t *testing.T) {
	cfg := bucketModelRoutesTestConfig(true)
	cases := []struct {
		name         string
		bucket       string
		model        string
		wantProvider string
		wantTo       string
		wantOK       bool
	}{
		{"default rule matches unmapped key", "", "gpt-5.6-sol", "antigravity", "gemini-3.8-flash-high", true},
		{"empty bucket rule also means default", "", "gpt-5.6-terra", "antigravity", "gemini-3.8-flash-high", true},
		{"named bucket rule matches its bucket", "team-a", "gpt-5.6-sol", "deepseek", "deepseek-chat", true},
		{"named bucket without rule is untouched", "team-b", "gpt-5.6-sol", "", "", false},
		{"unmatched model is untouched", "", "gpt-5.5", "", "", false},
		{"requested model is trimmed", "", " gpt-5.6-sol ", "antigravity", "gemini-3.8-flash-high", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, to, ok := cfg.CodexBucketModelRoute(tc.bucket, tc.model)
			if ok != tc.wantOK || provider != tc.wantProvider || to != tc.wantTo {
				t.Fatalf("CodexBucketModelRoute(%q, %q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.bucket, tc.model, provider, to, ok, tc.wantProvider, tc.wantTo, tc.wantOK)
			}
		})
	}
}

func TestValidateCodexBucketModelRoutes(t *testing.T) {
	if err := bucketModelRoutesTestConfig(true).ValidateCodexBucketModelRoutes(); err != nil {
		t.Fatalf("valid rules rejected: %v", err)
	}
	if err := (&SDKConfig{}).ValidateCodexBucketModelRoutes(); err != nil {
		t.Fatalf("empty config rejected: %v", err)
	}
	bad := []struct {
		name  string
		rules []CodexBucketModelRoute
	}{
		{"missing from", []CodexBucketModelRoute{{Bucket: "default", Provider: "antigravity", To: "x"}}},
		{"missing provider", []CodexBucketModelRoute{{Bucket: "default", From: "a", To: "x"}}},
		{"missing to", []CodexBucketModelRoute{{Bucket: "default", From: "a", Provider: "antigravity"}}},
		{"duplicate bucket+from", []CodexBucketModelRoute{
			{Bucket: "default", From: "a", Provider: "antigravity", To: "x"},
			{Bucket: "", From: "a", Provider: "deepseek", To: "y"},
		}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &SDKConfig{CodexBucketModelRoutes: CodexBucketModelRoutes{Enabled: true, Rules: tc.rules}}
			if err := cfg.ValidateCodexBucketModelRoutes(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestLoadConfigCodexBucketModelRoutes(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	valid := "codex-bucket-model-routes:\n  enabled: true\n  rules:\n    - bucket: default\n      from: gpt-5.6-sol\n      provider: antigravity\n      to: gemini-3.8-flash-high\n"
	if err := os.WriteFile(configPath, []byte(valid), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	provider, to, ok := cfg.CodexBucketModelRoute("", "gpt-5.6-sol")
	if !ok || provider != "antigravity" || to != "gemini-3.8-flash-high" {
		t.Fatalf("loaded route = (%q, %q, %v)", provider, to, ok)
	}

	invalid := "codex-bucket-model-routes:\n  enabled: true\n  rules:\n    - bucket: default\n      from: gpt-5.6-sol\n      provider: antigravity\n"
	if err := os.WriteFile(configPath, []byte(invalid), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadConfig(configPath); err == nil {
		t.Fatal("LoadConfig must reject a rule without to")
	}
}
