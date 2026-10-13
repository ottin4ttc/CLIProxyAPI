package auth

import (
	"context"
	"testing"
)

func TestAuthBucket(t *testing.T) {
	if got := authBucket(nil); got != "" {
		t.Fatalf("nil auth = %q, want empty", got)
	}
	if got := authBucket(&Auth{}); got != "" {
		t.Fatalf("no bucket = %q, want empty", got)
	}
	attr := &Auth{Attributes: map[string]string{AttributeBucket: " team-a "}}
	if got := authBucket(attr); got != "team-a" {
		t.Fatalf("attribute bucket = %q, want team-a", got)
	}
	meta := &Auth{Metadata: map[string]any{AttributeBucket: "team-b"}}
	if got := authBucket(meta); got != "team-b" {
		t.Fatalf("metadata bucket = %q, want team-b", got)
	}
	// Empty attribute falls through to metadata; non-string metadata is ignored.
	fallthroughAuth := &Auth{
		Attributes: map[string]string{AttributeBucket: "  "},
		Metadata:   map[string]any{AttributeBucket: "team-c"},
	}
	if got := authBucket(fallthroughAuth); got != "team-c" {
		t.Fatalf("fallthrough bucket = %q, want team-c", got)
	}
	nonString := &Auth{Metadata: map[string]any{AttributeBucket: 42}}
	if got := authBucket(nonString); got != "" {
		t.Fatalf("non-string metadata = %q, want empty", got)
	}
}

func TestEligibilityBucket(t *testing.T) {
	codexIn := &Auth{Provider: "codex", Metadata: map[string]any{AttributeBucket: "team-a"}}
	codexOther := &Auth{Provider: "codex", Metadata: map[string]any{AttributeBucket: "team-b"}}
	codexDefault := &Auth{Provider: "codex"}
	gemini := &Auth{Provider: "gemini"}

	bucketed := authSelectionEligibility{bucket: "team-a"}
	if !bucketed.allows(codexIn) {
		t.Fatal("bucketed request must allow same-bucket codex auth")
	}
	if bucketed.allows(codexOther) {
		t.Fatal("bucketed request must reject other-bucket codex auth")
	}
	if bucketed.allows(codexDefault) {
		t.Fatal("bucketed request must reject unbucketed codex auth")
	}
	if !bucketed.allows(gemini) {
		t.Fatal("bucket filter must not affect non-codex providers")
	}

	unmapped := authSelectionEligibility{}
	if !unmapped.allows(codexDefault) {
		t.Fatal("unmapped request must allow unbucketed codex auth")
	}
	if unmapped.allows(codexIn) {
		t.Fatal("unmapped request must reject bucketed codex auth")
	}
	if !unmapped.allows(gemini) {
		t.Fatal("unmapped request must allow non-codex providers")
	}
}

func TestEligibilityClaudeBucket(t *testing.T) {
	claudeIn := &Auth{Provider: "claude", Metadata: map[string]any{AttributeBucket: "team-a"}}
	claudeDefault := &Auth{Provider: "claude"}
	antigravityTagged := &Auth{Provider: "antigravity", Metadata: map[string]any{AttributeBucket: "team-a"}}

	bucketed := authSelectionEligibility{bucket: "team-a"}
	if !bucketed.allows(claudeIn) {
		t.Fatal("bucketed request must allow same-bucket claude auth")
	}
	if bucketed.allows(claudeDefault) {
		t.Fatal("bucketed request must reject unbucketed claude auth")
	}

	unmapped := authSelectionEligibility{}
	if unmapped.allows(claudeIn) {
		t.Fatal("unmapped request must reject bucketed claude auth")
	}
	if !unmapped.allows(claudeDefault) {
		t.Fatal("unmapped request must allow unbucketed claude auth")
	}
	if !unmapped.allows(antigravityTagged) {
		t.Fatal("bucket tags must be ignored for providers that are not bucket-scoped")
	}
}

func TestBucketFromMetadata(t *testing.T) {
	if got := bucketFromMetadata(nil); got != "" {
		t.Fatalf("nil meta = %q, want empty", got)
	}
	meta := map[string]any{"bucket": " team-a "}
	if got := bucketFromMetadata(meta); got != "team-a" {
		t.Fatalf("meta bucket = %q, want team-a", got)
	}
	if got := bucketFromMetadata(map[string]any{"bucket": 7}); got != "" {
		t.Fatalf("non-string = %q, want empty", got)
	}
}

func TestBuildBucketIndex(t *testing.T) {
	idx := buildBucketIndex(map[string]*Auth{
		"codex-a":      {Provider: "codex", Metadata: map[string]any{"access_token": "t", AttributeBucket: "team-a"}},
		"codex-off":    {Provider: "Codex", Disabled: true, Metadata: map[string]any{"access_token": "t", AttributeBucket: "team-off"}},
		"claude-key":   {Provider: "claude", Attributes: map[string]string{"api_key": "k", AttributeBucket: "team-k"}},
		"antigravity":  {Provider: "antigravity", Metadata: map[string]any{"access_token": "t", AttributeBucket: "team-g"}},
		"codex-plain":  {Provider: "codex", Metadata: map[string]any{"access_token": "t"}},
		"claude-oauth": {Provider: "claude", Metadata: map[string]any{"access_token": "t", AttributeBucket: "team-a"}},
	})
	for _, tc := range []struct {
		provider, bucket string
		want             bool
	}{
		{"codex", "team-a", true},
		{"claude", "team-a", true},
		{"codex", "team-off", true}, // disabled accounts still own their bucket
		{"claude", "team-k", false}, // API keys are never bucketed
		{"antigravity", "team-g", false},
		{"claude", "team-off", false},
	} {
		if got := idx.has(tc.provider, tc.bucket); got != tc.want {
			t.Errorf("has(%s, %s) = %v, want %v", tc.provider, tc.bucket, got, tc.want)
		}
	}
}

func TestManagerBucketIndexTracksRegisterAndRemove(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	if manager.loadBucketIndex() != nil {
		t.Fatal("bucket index must be nil before any auth is registered")
	}
	tagged := &Auth{ID: "codex-a", Provider: "codex", Metadata: map[string]any{"access_token": "t", AttributeBucket: "team-a"}}
	if _, errRegister := manager.Register(context.Background(), tagged); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	if !manager.loadBucketIndex().has("codex", "team-a") {
		t.Fatal("bucket index must include team-a after Register")
	}
	manager.Remove(context.Background(), "codex-a")
	if manager.loadBucketIndex().has("codex", "team-a") {
		t.Fatal("bucket index must drop team-a after Remove")
	}
}
