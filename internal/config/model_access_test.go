package config

import "testing"

func TestModelAccessAllowedDisabledIgnoresRules(t *testing.T) {
	cfg := &SDKConfig{ModelAccess: ModelAccess{
		Enabled: false,
		Rules:   []ModelAccessRule{{Models: []string{"deepseek-*"}, APIKeys: []string{"sk-a"}}},
	}}
	if !cfg.ModelAccessAllowed("sk-other", "deepseek-flash") {
		t.Fatal("disabled rules must allow every key")
	}
	var nilCfg *SDKConfig
	if !nilCfg.ModelAccessAllowed("sk-other", "deepseek-flash") {
		t.Fatal("nil receiver must allow")
	}
}

func TestModelAccessAllowedEnabled(t *testing.T) {
	cfg := &SDKConfig{ModelAccess: ModelAccess{
		Enabled: true,
		Rules: []ModelAccessRule{
			{Models: []string{"deepseek-*"}, APIKeys: []string{" sk-a ", "sk-b"}},
			{Models: []string{"exact-model"}, APIKeys: []string{"sk-c"}},
			{Models: []string{"*-locked"}, APIKeys: nil},
		},
	}}
	cases := []struct {
		name   string
		apiKey string
		model  string
		want   bool
	}{
		{"unmatched model is open to anyone", "sk-other", "gpt-5.5", true},
		{"listed key passes wildcard rule", "sk-a", "deepseek-flash", true},
		{"configured key is trimmed", "sk-b", "deepseek-v4-pro", true},
		{"unlisted key is denied", "sk-other", "deepseek-flash", false},
		{"empty key is denied on matched model", "", "deepseek-flash", false},
		{"exact rule matches exactly", "sk-c", "exact-model", true},
		{"exact rule does not match superstring", "sk-other", "exact-model-2", true},
		{"provider prefix is stripped before matching", "sk-other", "ds/deepseek-flash", false},
		{"provider prefix with listed key", "sk-a", "ds/deepseek-flash", true},
		{"rule with no keys locks the model", "sk-a", "anything-locked", false},
		{"key listed in one of two matching rules", "sk-c", "exact-model", true},
	}
	for _, tc := range cases {
		if got := cfg.ModelAccessAllowed(tc.apiKey, tc.model); got != tc.want {
			t.Fatalf("%s: ModelAccessAllowed(%q, %q) = %v, want %v", tc.name, tc.apiKey, tc.model, got, tc.want)
		}
	}
}

func TestMatchModelAccessPattern(t *testing.T) {
	cases := []struct {
		pattern string
		value   string
		want    bool
	}{
		{"", "x", false},
		{"a", "a", true},
		{"a", "ab", false},
		{"a*", "abc", true},
		{"*c", "abc", true},
		{"*b*", "abc", true},
		{"a*c", "abc", true},
		{"a*c", "ac", true},
		{"a*c", "ab", false},
		{"*", "", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "acb", false},
	}
	for _, tc := range cases {
		if got := matchModelAccessPattern(tc.pattern, tc.value); got != tc.want {
			t.Fatalf("matchModelAccessPattern(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
}
