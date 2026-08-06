// Package store implements conversation recording for the CLIProxyAPI
// conversation-store plugin: session keying, request/response correlation,
// JSONL persistence, archiving, and retention.
package convstore

import "gopkg.in/yaml.v3"

// Config holds plugin settings decoded from the plugins.configs.conversation-store block.
type Config struct {
	DataDir                  string `yaml:"data-dir"`
	RetentionDays            int    `yaml:"retention-days"`
	ArchiveIdleHours         int    `yaml:"archive-idle-hours"`
	StreamIdleTimeoutSeconds int    `yaml:"stream-idle-timeout-seconds"`
	MaxBodyBytes             int    `yaml:"max-body-bytes"`
}

// ParseConfig decodes the plugin YAML config, filling documented defaults
// for keys that are absent. retention-days: 0 is a valid override meaning
// "never purge".
func ParseConfig(configYAML []byte) (Config, error) {
	cfg := Config{
		DataDir:                  "conversations",
		RetentionDays:            14,
		ArchiveIdleHours:         6,
		StreamIdleTimeoutSeconds: 120,
		MaxBodyBytes:             2097152,
	}
	if len(configYAML) == 0 {
		return cfg, nil
	}
	if err := yaml.Unmarshal(configYAML, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "conversations"
	}
	if cfg.StreamIdleTimeoutSeconds <= 0 {
		cfg.StreamIdleTimeoutSeconds = 120
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 2097152
	}
	return cfg, nil
}
