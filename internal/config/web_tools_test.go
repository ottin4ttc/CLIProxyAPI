package config

import "testing"

func TestParseConfigBytesWebToolsDefaultsOff(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`port: 8317
`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if cfg.WebTools.Enabled {
		t.Fatal("web-tools must stay disabled when the section is absent")
	}
}

func TestParseConfigBytesWebTools(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`port: 8317
web-tools:
  enabled: true
  base-url: " https://api.deepseek.com/v1/tools "
  credential: " deepseek "
  count: 99
  need-content: true
`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if !cfg.WebTools.Enabled {
		t.Fatal("web-tools.enabled was not parsed")
	}
	if cfg.WebTools.BaseURL != "https://api.deepseek.com/v1/tools" {
		t.Fatalf("base-url = %q, want trimmed value", cfg.WebTools.BaseURL)
	}
	if cfg.WebTools.Credential != "deepseek" {
		t.Fatalf("credential = %q, want trimmed value", cfg.WebTools.Credential)
	}
	if cfg.WebTools.Count != 50 {
		t.Fatalf("count = %d, want clamped 50", cfg.WebTools.Count)
	}
	if !cfg.WebTools.NeedContent {
		t.Fatal("need-content was not parsed")
	}
}
