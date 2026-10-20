package auth

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestFilterVisibleModelsHonorsBucketAndExclusions(t *testing.T) {
	const clientID = "test-visibility-client"
	const model = "test-visibility-model"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{ID: model, Object: "model"}})
	t.Cleanup(func() { reg.UnregisterClient(clientID) })

	m := &Manager{auths: map[string]*Auth{
		"sub": {
			ID:       "sub",
			Provider: "claude",
			Attributes: map[string]string{
				AttributeAuthKind: AuthKindOAuth,
				AttributeBucket:   "claude-sub",
			},
		},
		"baidu": {
			ID:         "baidu",
			Provider:   "claude",
			Attributes: map[string]string{AttributeAPIKey: "sk-upstream"},
		},
	}}
	models := []string{model}

	tests := []struct {
		name    string
		bucket  string
		exclude map[string][]string
		want    bool
	}{
		{name: "bucket member sees subscription", bucket: "claude-sub", want: true},
		{name: "unmapped key still sees apikey channel", bucket: "", want: true},
		{name: "apikey excluded outside bucket hides model", bucket: "", exclude: map[string][]string{model: {"apikey"}}, want: false},
		{name: "apikey excluded inside bucket keeps subscription", bucket: "claude-sub", exclude: map[string][]string{model: {"apikey"}}, want: true},
	}
	for _, tc := range tests {
		got := m.FilterVisibleModels(models, tc.bucket, tc.exclude)
		if got[model] != tc.want {
			t.Fatalf("%s: visible = %t, want %t", tc.name, got[model], tc.want)
		}
	}
}

func TestUnbucketedOAuthAuthIDs(t *testing.T) {
	m := &Manager{auths: map[string]*Auth{
		"tagged": {
			ID:         "tagged",
			Provider:   "claude",
			Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth, AttributeBucket: "sub"},
		},
		"untagged": {
			ID:         "untagged",
			Provider:   "claude",
			Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth},
		},
		"apikey": {
			ID:         "apikey",
			Provider:   "claude",
			Attributes: map[string]string{AttributeAPIKey: "sk-upstream"},
		},
	}}
	got := m.UnbucketedOAuthAuthIDs("claude")
	if len(got) != 1 || got[0] != "untagged" {
		t.Fatalf("UnbucketedOAuthAuthIDs() = %#v, want [untagged]", got)
	}
}
