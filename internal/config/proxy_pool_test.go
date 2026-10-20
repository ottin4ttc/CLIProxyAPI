package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProxyPoolParsingAndResolution(t *testing.T) {
	t.Setenv("PROXY_POOL_TEST_PASSWORD", "s3cret")

	raw := `
proxy-pool:
  us-1: "socks5h://127.0.0.1:1081"
  resi-1:
    url: "socks5h://user@1.2.3.4:1080"
    password-env: "PROXY_POOL_TEST_PASSWORD"
    timezone: "America/New_York"
  direct: "direct"
`
	var cfg Config
	if errUnmarshal := yaml.Unmarshal([]byte(raw), &cfg); errUnmarshal != nil {
		t.Fatalf("unmarshal proxy-pool: %v", errUnmarshal)
	}
	cfg.SanitizeProxyPool()

	if got, ok := cfg.ProxyPool.Resolve("us-1"); !ok || got != "socks5h://127.0.0.1:1081" {
		t.Fatalf("resolve us-1 = %q, %v", got, ok)
	}
	if got, ok := cfg.ProxyPool.Resolve("resi-1"); !ok || !strings.Contains(got, "user:s3cret@") {
		t.Fatalf("resolve resi-1 = %q, %v", got, ok)
	}
	if got, ok := cfg.ProxyPool.Resolve("direct"); !ok || got != "direct" {
		t.Fatalf("resolve direct = %q, %v", got, ok)
	}

	resolved, errResolve := cfg.ResolveLoginProxy("resi-1")
	if errResolve != nil || resolved.Ref != "resi-1" || !strings.Contains(resolved.URL, "user:s3cret@") || resolved.Timezone != "America/New_York" {
		t.Fatalf("ResolveLoginProxy(resi-1) = %#v, %v", resolved, errResolve)
	}
	literal, errLiteral := cfg.ResolveLoginProxy("socks5://127.0.0.1:9999")
	if errLiteral != nil || literal.Ref != "" || literal.Timezone != "" || literal.URL != "socks5://127.0.0.1:9999" {
		t.Fatalf("ResolveLoginProxy(url) = %#v, %v", literal, errLiteral)
	}
	if _, errMissing := cfg.ResolveLoginProxy("missing"); errMissing == nil {
		t.Fatal("ResolveLoginProxy(missing) succeeded, want error")
	}
}

func TestProxyPoolSanitizeDropsUnusableEntries(t *testing.T) {
	cfg := &Config{SDKConfig: SDKConfig{ProxyPool: ProxyPool{
		"bad-scheme": {URL: "ftp://127.0.0.1:21"},
		"no-user":    {URL: "socks5://127.0.0.1:1080", PasswordEnv: "SOME_PASSWORD"},
		"empty":      {URL: "  "},
		"ok":         {URL: " http://127.0.0.1:8080 "},
	}}}
	cfg.SanitizeProxyPool()

	if len(cfg.ProxyPool) != 1 {
		t.Fatalf("proxy pool = %#v, want only the valid entry", cfg.ProxyPool)
	}
	if got, ok := cfg.ProxyPool.Resolve("ok"); !ok || got != "http://127.0.0.1:8080" {
		t.Fatalf("resolve ok = %q, %v", got, ok)
	}
}

func TestProxyPoolMissingPasswordEnvIsNotUsable(t *testing.T) {
	t.Setenv("PROXY_POOL_EMPTY_PASSWORD", "")
	cfg := &Config{SDKConfig: SDKConfig{ProxyPool: ProxyPool{
		"resi": {URL: "socks5://user@1.2.3.4:1080", PasswordEnv: "PROXY_POOL_EMPTY_PASSWORD"},
	}}}
	cfg.SanitizeProxyPool()

	if _, ok := cfg.ProxyPool.Resolve("resi"); ok {
		t.Fatal("resolve succeeded with an unset password-env")
	}
	if _, errResolve := cfg.ResolveLoginProxy("resi"); errResolve == nil {
		t.Fatal("ResolveLoginProxy succeeded with an unset password-env")
	}
}

func TestProxyPoolInvalidTimezoneIsIgnored(t *testing.T) {
	cfg := &Config{SDKConfig: SDKConfig{ProxyPool: ProxyPool{
		"us": {URL: "socks5://127.0.0.1:1080", Timezone: "Mars/Olympus"},
	}}}
	cfg.SanitizeProxyPool()

	resolved, errResolve := cfg.ResolveLoginProxy("us")
	if errResolve != nil || resolved.URL != "socks5://127.0.0.1:1080" || resolved.Timezone != "" {
		t.Fatalf("ResolveLoginProxy(us) = %#v, %v", resolved, errResolve)
	}
}
