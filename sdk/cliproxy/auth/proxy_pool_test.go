package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func proxyPoolConfig(pool internalconfig.ProxyPool) *internalconfig.Config {
	return &internalconfig.Config{SDKConfig: internalconfig.SDKConfig{ProxyPool: pool}}
}

func TestApplyProxyPoolRefResolvesAndFallsBack(t *testing.T) {
	cfg := proxyPoolConfig(internalconfig.ProxyPool{
		"us-1": {URL: "socks5h://127.0.0.1:1081"},
	})

	resolved := &Auth{Metadata: map[string]any{
		"proxy_ref": "us-1",
		"proxy_url": "socks5h://127.0.0.1:9999",
	}}
	applyProxyPoolRef(resolved, cfg)
	if resolved.ProxyURL != "socks5h://127.0.0.1:1081" {
		t.Fatalf("ProxyURL = %q, want the pool url", resolved.ProxyURL)
	}

	missing := &Auth{Metadata: map[string]any{
		"proxy_ref": "gone",
		"proxy_url": "socks5h://127.0.0.1:9999",
	}}
	applyProxyPoolRef(missing, cfg)
	if missing.ProxyURL != "socks5h://127.0.0.1:9999" {
		t.Fatalf("ProxyURL = %q, want the persisted fallback", missing.ProxyURL)
	}

	noRef := &Auth{Metadata: map[string]any{"proxy_url": "socks5h://127.0.0.1:9999"}}
	applyProxyPoolRef(noRef, cfg)
	if noRef.ProxyURL != "" {
		t.Fatalf("ProxyURL = %q, want unchanged for an auth without proxy_ref", noRef.ProxyURL)
	}
}

func TestManagerSetConfigAppliesProxyPoolRef(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "claude-a",
		Provider: "claude",
		ProxyURL: "socks5h://127.0.0.1:9999",
		Metadata: map[string]any{
			"proxy_ref": "us-1",
			"proxy_url": "socks5h://127.0.0.1:9999",
		},
	})
	if errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	if registered.ProxyURL != "socks5h://127.0.0.1:9999" {
		t.Fatalf("registered ProxyURL = %q, want the persisted fallback", registered.ProxyURL)
	}

	manager.SetConfig(proxyPoolConfig(internalconfig.ProxyPool{
		"us-1": {URL: "socks5h://127.0.0.1:1081"},
	}))
	if got := manager.auths["claude-a"].ProxyURL; got != "socks5h://127.0.0.1:1081" {
		t.Fatalf("after SetConfig ProxyURL = %q, want the pool url", got)
	}

	manager.SetConfig(proxyPoolConfig(internalconfig.ProxyPool{
		"us-1": {URL: "socks5h://127.0.0.1:1082"},
	}))
	if got := manager.auths["claude-a"].ProxyURL; got != "socks5h://127.0.0.1:1082" {
		t.Fatalf("after pool rotation ProxyURL = %q, want the new pool url", got)
	}
}
