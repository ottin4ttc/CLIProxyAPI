package handlers

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func bucketModelRouteTestConfig(enabled bool) *config.SDKConfig {
	return &config.SDKConfig{
		CodexBuckets: map[string]config.CodexBucket{"anon": {APIKeys: []string{"sk-anon"}}},
		CodexBucketModelRoutes: config.CodexBucketModelRoutes{
			Enabled: enabled,
			Rules: []config.CodexBucketModelRoute{
				{Bucket: "default", From: "gpt-5.6-sol", Provider: "antigravity", To: "gemini-3.8-flash-high"},
			},
		},
	}
}

func bucketModelRouteTestContext(apiKey string) context.Context {
	if apiKey == "" {
		return context.Background()
	}
	return context.WithValue(context.Background(), "gin", modelAccessTestGinContext(apiKey))
}

func TestApplyModelRouterBucketRouteRewritesDefaultBucketKey(t *testing.T) {
	h := &BaseAPIHandler{Cfg: bucketModelRouteTestConfig(true)}
	decision := h.applyModelRouter(bucketModelRouteTestContext("sk-default"), "openai", "gpt-5.6-sol", nil, true, modelExecutionOptions{})
	if decision.Provider != "antigravity" || decision.Model != "gemini-3.8-flash-high" || decision.ExecutorPluginID != "" {
		t.Fatalf("decision = %+v, want provider antigravity model gemini-3.8-flash-high", decision)
	}
	providers, model, errMsg := h.providersForExecution("gpt-5.6-sol", "gpt-5.6-sol", false, decision, modelExecutionOptions{})
	if errMsg != nil {
		t.Fatalf("providersForExecution error: %v", errMsg.Error)
	}
	if len(providers) != 1 || providers[0] != "antigravity" || model != "gemini-3.8-flash-high" {
		t.Fatalf("providersForExecution = (%v, %q), want ([antigravity], gemini-3.8-flash-high)", providers, model)
	}
}

func TestApplyModelRouterBucketRouteLeavesOtherRequestsAlone(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *config.SDKConfig
		apiKey  string
		model   string
		wantHit bool
	}{
		{"key in another bucket", bucketModelRouteTestConfig(true), "sk-anon", "gpt-5.6-sol", false},
		{"unmatched model", bucketModelRouteTestConfig(true), "sk-default", "gpt-5.5", false},
		{"no client api key", bucketModelRouteTestConfig(true), "", "gpt-5.6-sol", false},
		{"routing disabled", bucketModelRouteTestConfig(false), "sk-default", "gpt-5.6-sol", false},
		{"nil config", nil, "sk-default", "gpt-5.6-sol", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &BaseAPIHandler{Cfg: tc.cfg}
			decision := h.applyModelRouter(bucketModelRouteTestContext(tc.apiKey), "openai", tc.model, nil, true, modelExecutionOptions{})
			if got := decision.Provider != "" || decision.Model != "" || decision.ExecutorPluginID != ""; got != tc.wantHit {
				t.Fatalf("decision = %+v, want hit=%v", decision, tc.wantHit)
			}
		})
	}
}

func TestApplyModelRouterPluginRouteWinsOverBucketRoute(t *testing.T) {
	host := &handlerModelRouterTestHost{
		hasRouters: true,
		route: func(context.Context, pluginapi.ModelRouteRequest, string) (pluginapi.ModelRouteResponse, bool) {
			return pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetProvider, Target: "gemini", TargetModel: "gemini-2.5-pro"}, true
		},
	}
	h := &BaseAPIHandler{Cfg: bucketModelRouteTestConfig(true), ModelRouterHost: host}
	decision := h.applyModelRouter(bucketModelRouteTestContext("sk-default"), "openai", "gpt-5.6-sol", nil, true, modelExecutionOptions{})
	if decision.Provider != "gemini" || decision.Model != "gemini-2.5-pro" {
		t.Fatalf("decision = %+v, want the plugin route", decision)
	}
}

func TestApplyModelRouterBucketRouteAppliesWhenPluginRouterDeclines(t *testing.T) {
	host := &handlerModelRouterTestHost{hasRouters: true}
	h := &BaseAPIHandler{Cfg: bucketModelRouteTestConfig(true), ModelRouterHost: host}
	decision := h.applyModelRouter(bucketModelRouteTestContext("sk-default"), "openai", "gpt-5.6-sol", nil, true, modelExecutionOptions{})
	if decision.Provider != "antigravity" || decision.Model != "gemini-3.8-flash-high" {
		t.Fatalf("decision = %+v, want the bucket route", decision)
	}
}
