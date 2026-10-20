package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type proxyCaptureAuthenticator struct {
	provider     string
	seenProxyURL string
}

func (a *proxyCaptureAuthenticator) Provider() string { return a.provider }

func (a *proxyCaptureAuthenticator) RefreshLead() *time.Duration { return nil }

func (a *proxyCaptureAuthenticator) Login(_ context.Context, cfg *config.Config, _ *LoginOptions) (*coreauth.Auth, error) {
	if cfg != nil {
		a.seenProxyURL = cfg.ProxyURL
	}
	return &coreauth.Auth{
		ID:       "proxy-login.json",
		FileName: "proxy-login.json",
		Provider: a.provider,
		Metadata: map[string]any{
			"type":         a.provider,
			"email":        "user@example.com",
			"access_token": "token",
		},
	}, nil
}

func TestManagerLoginAppliesAndPersistsProxy(t *testing.T) {
	authDir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(authDir)

	authenticator := &proxyCaptureAuthenticator{provider: "demo"}
	manager := NewManager(store, authenticator)

	cfg := &config.Config{AuthDir: authDir, SDKConfig: config.SDKConfig{ProxyURL: "http://global.example:8080"}}
	_, savedPath, errLogin := manager.Login(context.Background(), "demo", cfg, &LoginOptions{
		ProxyURL: "socks5h://127.0.0.1:1081",
		ProxyRef: "us-1",
		Timezone: "America/New_York",
	})
	if errLogin != nil {
		t.Fatalf("login: %v", errLogin)
	}
	if authenticator.seenProxyURL != "socks5h://127.0.0.1:1081" {
		t.Fatalf("login proxy = %q, want the per-login override", authenticator.seenProxyURL)
	}
	if cfg.ProxyURL != "http://global.example:8080" {
		t.Fatalf("original config proxy = %q, want it unmodified", cfg.ProxyURL)
	}

	raw, errRead := os.ReadFile(savedPath)
	if errRead != nil {
		t.Fatalf("read saved auth: %v", errRead)
	}
	var saved map[string]any
	if errUnmarshal := json.Unmarshal(raw, &saved); errUnmarshal != nil {
		t.Fatalf("decode saved auth: %v", errUnmarshal)
	}
	if saved["proxy_url"] != "socks5h://127.0.0.1:1081" {
		t.Fatalf("saved proxy_url = %v", saved["proxy_url"])
	}
	if saved["proxy_ref"] != "us-1" {
		t.Fatalf("saved proxy_ref = %v", saved["proxy_ref"])
	}
	if saved["timezone"] != "America/New_York" {
		t.Fatalf("saved timezone = %v", saved["timezone"])
	}
}

func TestManagerLoginExplicitProxyURLClearsStaleRef(t *testing.T) {
	authDir := t.TempDir()
	fileName := "proxy-login.json"
	existing := map[string]any{
		"type":         "demo",
		"email":        "user@example.com",
		"proxy_url":    "socks5h://127.0.0.1:9999",
		"proxy_ref":    "us-9",
		"access_token": "old",
	}
	raw, errMarshal := json.Marshal(existing)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if errWrite := os.WriteFile(filepath.Join(authDir, fileName), raw, 0o600); errWrite != nil {
		t.Fatalf("write existing: %v", errWrite)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := NewManager(store, &proxyCaptureAuthenticator{provider: "demo"})

	_, savedPath, errLogin := manager.Login(context.Background(), "demo", &config.Config{AuthDir: authDir}, &LoginOptions{
		ProxyURL: "socks5h://127.0.0.1:1081",
	})
	if errLogin != nil {
		t.Fatalf("login: %v", errLogin)
	}
	savedRaw, errRead := os.ReadFile(savedPath)
	if errRead != nil {
		t.Fatalf("read saved auth: %v", errRead)
	}
	var saved map[string]any
	if errUnmarshal := json.Unmarshal(savedRaw, &saved); errUnmarshal != nil {
		t.Fatalf("decode saved auth: %v", errUnmarshal)
	}
	if saved["proxy_url"] != "socks5h://127.0.0.1:1081" {
		t.Fatalf("saved proxy_url = %v, want the explicit URL", saved["proxy_url"])
	}
	if saved["proxy_ref"] != "" {
		t.Fatalf("saved proxy_ref = %v, want it cleared", saved["proxy_ref"])
	}
}

func TestManagerLoginKeepsExistingProxyWhenNoOverride(t *testing.T) {
	authDir := t.TempDir()
	fileName := "proxy-login.json"
	existing := map[string]any{
		"type":         "demo",
		"email":        "user@example.com",
		"proxy_url":    "socks5h://127.0.0.1:9999",
		"proxy_ref":    "us-9",
		"access_token": "old",
	}
	raw, errMarshal := json.Marshal(existing)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if errWrite := os.WriteFile(filepath.Join(authDir, fileName), raw, 0o600); errWrite != nil {
		t.Fatalf("write existing: %v", errWrite)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := NewManager(store, &proxyCaptureAuthenticator{provider: "demo"})

	_, savedPath, errLogin := manager.Login(context.Background(), "demo", &config.Config{AuthDir: authDir}, &LoginOptions{})
	if errLogin != nil {
		t.Fatalf("login: %v", errLogin)
	}
	savedRaw, errRead := os.ReadFile(savedPath)
	if errRead != nil {
		t.Fatalf("read saved auth: %v", errRead)
	}
	var saved map[string]any
	if errUnmarshal := json.Unmarshal(savedRaw, &saved); errUnmarshal != nil {
		t.Fatalf("decode saved auth: %v", errUnmarshal)
	}
	if saved["proxy_url"] != "socks5h://127.0.0.1:9999" || saved["proxy_ref"] != "us-9" {
		t.Fatalf("existing proxy metadata was not preserved: proxy_url=%v proxy_ref=%v", saved["proxy_url"], saved["proxy_ref"])
	}
}
