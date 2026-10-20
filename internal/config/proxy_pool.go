package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// ProxyPool maps operator-defined names to upstream proxies. A credential can
// record a proxy_ref that resolves through this pool at runtime, and the
// --login-proxy flag accepts a pool name so proxy credentials are configured
// once instead of per login.
type ProxyPool map[string]ProxyPoolEntry

// ProxyPoolEntry is one named upstream proxy.
type ProxyPoolEntry struct {
	// URL is the proxy URL (http, https, socks5, socks5h, or direct/none).
	URL string `yaml:"url" json:"url"`
	// PasswordEnv names an environment variable that supplies the proxy
	// password. It is read at use time, so the secret does not have to live
	// in the configuration file.
	PasswordEnv string `yaml:"password-env,omitempty" json:"password-env,omitempty"`
	// Timezone is the IANA timezone matching the proxy's exit region. It is
	// stored on credentials created through --login-proxy and drives the
	// cloaked currentDate reminder, so login and date agree with the egress.
	Timezone string `yaml:"timezone,omitempty" json:"timezone,omitempty"`
}

// UnmarshalYAML accepts either a plain proxy URL or a mapping with url and
// password-env.
func (e *ProxyPoolEntry) UnmarshalYAML(value *yaml.Node) error {
	if value == nil {
		return nil
	}
	if value.Kind == yaml.ScalarNode {
		e.URL = strings.TrimSpace(value.Value)
		e.PasswordEnv = ""
		e.Timezone = ""
		return nil
	}
	type plain ProxyPoolEntry
	var decoded plain
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	*e = ProxyPoolEntry(decoded)
	return nil
}

// Resolve returns the proxy URL for a pool name, applying password-env when
// configured. Reports false when the name is unknown, the URL is empty, or the
// configured password environment variable is unset.
func (p ProxyPool) Resolve(name string) (string, bool) {
	entry, ok := p[strings.TrimSpace(name)]
	if !ok {
		return "", false
	}
	raw := strings.TrimSpace(entry.URL)
	if raw == "" {
		return "", false
	}
	if entry.PasswordEnv == "" {
		return raw, true
	}
	password := os.Getenv(strings.TrimSpace(entry.PasswordEnv))
	if password == "" {
		return "", false
	}
	withPassword, okPassword := proxyURLWithPassword(raw, password)
	if !okPassword {
		return "", false
	}
	return withPassword, true
}

// proxyURLWithPassword replaces the password of a proxy URL that already
// carries a username.
func proxyURLWithPassword(raw, password string) (string, bool) {
	parsed, errParse := url.Parse(raw)
	if errParse != nil || parsed.User == nil || parsed.User.Username() == "" {
		return "", false
	}
	parsed.User = url.UserPassword(parsed.User.Username(), password)
	return parsed.String(), true
}

// SanitizeProxyPool trims names and URLs and drops entries that cannot be used.
func (cfg *Config) SanitizeProxyPool() {
	if cfg == nil || len(cfg.ProxyPool) == 0 {
		return
	}
	out := make(ProxyPool, len(cfg.ProxyPool))
	for name, entry := range cfg.ProxyPool {
		entryName := strings.TrimSpace(name)
		raw := strings.TrimSpace(entry.URL)
		entry.PasswordEnv = strings.TrimSpace(entry.PasswordEnv)
		entry.Timezone = strings.TrimSpace(entry.Timezone)
		if entry.Timezone != "" {
			if _, errLocation := time.LoadLocation(entry.Timezone); errLocation != nil {
				log.Warnf("proxy-pool: ignoring invalid timezone %q for %q", entry.Timezone, entryName)
				entry.Timezone = ""
			}
		}
		if entryName == "" || raw == "" {
			log.Warn("proxy-pool: dropping entry with empty name or url")
			continue
		}
		setting, errParse := proxyutil.Parse(raw)
		if errParse != nil || (setting.Mode != proxyutil.ModeProxy && setting.Mode != proxyutil.ModeDirect) {
			log.Warnf("proxy-pool: dropping %q: unsupported proxy url", entryName)
			continue
		}
		if entry.PasswordEnv != "" && (setting.Mode != proxyutil.ModeProxy || setting.URL == nil || setting.URL.User == nil || setting.URL.User.Username() == "") {
			log.Warnf("proxy-pool: dropping %q: password-env requires a proxy url with a username", entryName)
			continue
		}
		entry.URL = raw
		out[entryName] = entry
	}
	cfg.ProxyPool = out
}

// LoginProxy is a resolved --login-proxy selection.
type LoginProxy struct {
	// URL is the proxy the login flow and the credential use.
	URL string
	// Ref is the proxy-pool name behind URL, empty for a literal URL.
	Ref string
	// Timezone is the pool entry's IANA timezone, empty when unknown.
	Timezone string
}

// ResolveLoginProxy maps a --login-proxy value to a proxy URL, the pool name it
// came from, and the entry's timezone. A value that is not a pool name must
// parse as a supported proxy URL (or direct/none).
func (cfg *Config) ResolveLoginProxy(value string) (LoginProxy, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return LoginProxy{}, nil
	}
	if cfg != nil {
		if entry, ok := cfg.ProxyPool[trimmed]; ok {
			resolved, okResolve := cfg.ProxyPool.Resolve(trimmed)
			if !okResolve {
				return LoginProxy{}, fmt.Errorf("login proxy %q is not usable: its password-env is unset or its url is invalid", trimmed)
			}
			return LoginProxy{URL: resolved, Ref: trimmed, Timezone: strings.TrimSpace(entry.Timezone)}, nil
		}
	}
	setting, errParse := proxyutil.Parse(trimmed)
	if errParse != nil || (setting.Mode != proxyutil.ModeProxy && setting.Mode != proxyutil.ModeDirect) {
		return LoginProxy{}, fmt.Errorf("login proxy %q is neither a proxy-pool name nor a supported proxy URL", trimmed)
	}
	return LoginProxy{URL: trimmed}, nil
}
