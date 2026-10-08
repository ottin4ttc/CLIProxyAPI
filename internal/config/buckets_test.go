package config

import "testing"

func TestBucketForAPIKey(t *testing.T) {
	cfg := &SDKConfig{Buckets: map[string]Bucket{
		"team-a": {APIKeys: []string{"sk-a1", "sk-a2"}},
		"team-b": {APIKeys: []string{"sk-b1"}},
	}}
	if got := cfg.BucketForAPIKey("sk-a2"); got != "team-a" {
		t.Fatalf("BucketForAPIKey(sk-a2) = %q, want team-a", got)
	}
	if got := cfg.BucketForAPIKey("sk-b1"); got != "team-b" {
		t.Fatalf("BucketForAPIKey(sk-b1) = %q, want team-b", got)
	}
	if got := cfg.BucketForAPIKey("sk-unmapped"); got != "" {
		t.Fatalf("BucketForAPIKey(sk-unmapped) = %q, want empty", got)
	}
	if got := cfg.BucketForAPIKey(""); got != "" {
		t.Fatalf("BucketForAPIKey(empty) = %q, want empty", got)
	}
	var nilCfg *SDKConfig
	if got := nilCfg.BucketForAPIKey("sk-a1"); got != "" {
		t.Fatalf("nil receiver = %q, want empty", got)
	}
}

func TestBucketForAPIKeyTrimsConfiguredKey(t *testing.T) {
	cfg := &SDKConfig{Buckets: map[string]Bucket{
		"team-a": {APIKeys: []string{" sk-a1 "}},
	}}
	if got := cfg.BucketForAPIKey("sk-a1"); got != "team-a" {
		t.Fatalf("BucketForAPIKey(sk-a1) = %q, want team-a (configured key should be trimmed)", got)
	}
	// The caller's key is compared as-is; a caller-supplied key with
	// surrounding whitespace should not match a trimmed configured key.
	if got := cfg.BucketForAPIKey(" sk-a1 "); got != "" {
		t.Fatalf("BucketForAPIKey(\" sk-a1 \") = %q, want empty (caller key is not trimmed)", got)
	}
}

func TestBucketForContextValue(t *testing.T) {
	cfg := &SDKConfig{Buckets: map[string]Bucket{
		"team-a": {APIKeys: []string{"sk-a1"}},
	}}
	if got := cfg.BucketForContextValue("sk-a1"); got != "team-a" {
		t.Fatalf("BucketForContextValue(sk-a1) = %q, want team-a", got)
	}
	if got := cfg.BucketForContextValue(nil); got != "" {
		t.Fatalf("BucketForContextValue(nil) = %q, want empty", got)
	}
	if got := cfg.BucketForContextValue("sk-unmapped"); got != "" {
		t.Fatalf("BucketForContextValue(sk-unmapped) = %q, want empty", got)
	}
	var nilCfg *SDKConfig
	if got := nilCfg.BucketForContextValue("sk-a1"); got != "" {
		t.Fatalf("nil receiver = %q, want empty", got)
	}
}

func TestValidateBucketsDuplicateKey(t *testing.T) {
	cfg := &SDKConfig{Buckets: map[string]Bucket{
		"team-a": {APIKeys: []string{"sk-dup"}},
		"team-b": {APIKeys: []string{"sk-dup"}},
	}}
	if err := cfg.ValidateBuckets(); err == nil {
		t.Fatal("expected error for api key mapped to two buckets")
	}
}

func TestValidateBucketsOK(t *testing.T) {
	cfg := &SDKConfig{Buckets: map[string]Bucket{
		// Same key twice inside one bucket is tolerated; empty entries ignored.
		"team-a": {APIKeys: []string{"sk-a1", "sk-a1", "  "}},
		"team-b": {APIKeys: []string{"sk-b1"}},
	}}
	if err := cfg.ValidateBuckets(); err != nil {
		t.Fatalf("ValidateBuckets: %v", err)
	}
	if err := (&SDKConfig{}).ValidateBuckets(); err != nil {
		t.Fatalf("empty config: %v", err)
	}
}

func TestValidateBucketsRejectsEmptyBucketName(t *testing.T) {
	cfg := &SDKConfig{Buckets: map[string]Bucket{
		"":   {APIKeys: []string{"sk-a1"}},
		"ok": {APIKeys: []string{"sk-b1"}},
	}}
	if err := cfg.ValidateBuckets(); err == nil {
		t.Fatal("expected error for empty bucket name")
	}
}

func TestValidateBucketsRejectsWhitespaceBucketName(t *testing.T) {
	cfg := &SDKConfig{Buckets: map[string]Bucket{
		"   ": {APIKeys: []string{"sk-a1"}},
	}}
	if err := cfg.ValidateBuckets(); err == nil {
		t.Fatal("expected error for whitespace-only bucket name")
	}
}
