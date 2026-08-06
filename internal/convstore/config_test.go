package convstore

import "testing"

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := ParseConfig(nil)
	if err != nil {
		t.Fatalf("ParseConfig(nil): %v", err)
	}
	if cfg.DataDir != "conversations" || cfg.RetentionDays != 14 ||
		cfg.ArchiveIdleHours != 6 || cfg.StreamIdleTimeoutSeconds != 120 ||
		cfg.MaxBodyBytes != 2097152 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestParseConfigOverrides(t *testing.T) {
	yamlSrc := []byte("data-dir: /var/conv\nretention-days: 0\nmax-body-bytes: 1024\n")
	cfg, err := ParseConfig(yamlSrc)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.DataDir != "/var/conv" || cfg.RetentionDays != 0 || cfg.MaxBodyBytes != 1024 {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if cfg.ArchiveIdleHours != 6 {
		t.Fatalf("unset key lost default: %+v", cfg)
	}
}

func TestParseConfigInvalidYAML(t *testing.T) {
	if _, err := ParseConfig([]byte(":\n:bad")); err == nil {
		t.Fatal("expected error for invalid yaml")
	}
}
