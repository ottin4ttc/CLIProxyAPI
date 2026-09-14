package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func modelAccessTestConfig(enabled bool) *config.SDKConfig {
	return &config.SDKConfig{ModelAccess: config.ModelAccess{
		Enabled: enabled,
		Rules:   []config.ModelAccessRule{{Models: []string{"deepseek-*"}, APIKeys: []string{"sk-allowed"}}},
	}}
}

func modelAccessTestGinContext(apiKey string) *gin.Context {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if apiKey != "" {
		ginCtx.Set("userApiKey", apiKey)
	}
	return ginCtx
}

func TestModelAccessErrorDeniesUnlistedKey(t *testing.T) {
	ctx := context.WithValue(context.Background(), "gin", modelAccessTestGinContext("sk-other"))
	errMsg := modelAccessError(ctx, modelAccessTestConfig(true), "deepseek-flash")
	if errMsg == nil {
		t.Fatal("unlisted key must be denied")
	}
	if errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("StatusCode = %d, want 403", errMsg.StatusCode)
	}
}

func TestModelAccessErrorAllowsListedKeyAndOpenModels(t *testing.T) {
	cfg := modelAccessTestConfig(true)
	ctx := context.WithValue(context.Background(), "gin", modelAccessTestGinContext("sk-allowed"))
	if errMsg := modelAccessError(ctx, cfg, "deepseek-flash"); errMsg != nil {
		t.Fatalf("listed key denied: %v", errMsg.Error)
	}
	ctx = context.WithValue(context.Background(), "gin", modelAccessTestGinContext("sk-other"))
	if errMsg := modelAccessError(ctx, cfg, "gpt-5.5"); errMsg != nil {
		t.Fatalf("unmatched model denied: %v", errMsg.Error)
	}
}

func TestModelAccessErrorDisabledOrNoGinContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), "gin", modelAccessTestGinContext("sk-other"))
	if errMsg := modelAccessError(ctx, modelAccessTestConfig(false), "deepseek-flash"); errMsg != nil {
		t.Fatalf("disabled rules must allow: %v", errMsg.Error)
	}
	if errMsg := modelAccessError(ctx, nil, "deepseek-flash"); errMsg != nil {
		t.Fatalf("nil config must allow: %v", errMsg.Error)
	}
	// A request without a gin context carries no key; a matched model is denied.
	if errMsg := modelAccessError(context.Background(), modelAccessTestConfig(true), "deepseek-flash"); errMsg == nil {
		t.Fatal("matched model without a client key must be denied")
	}
}

func TestFilterModelsForRequest(t *testing.T) {
	models := []map[string]any{
		{"id": "deepseek-flash"},
		{"id": "gpt-5.5"},
		{"name": "models/deepseek-v4-pro"},
		{"name": "gemini-3-pro"},
	}
	h := &BaseAPIHandler{Cfg: modelAccessTestConfig(true)}

	got := h.FilterModelsForRequest(modelAccessTestGinContext("sk-other"), models)
	if len(got) != 2 {
		t.Fatalf("unlisted key sees %d models, want 2: %v", len(got), got)
	}
	if got[0]["id"] != "gpt-5.5" || got[1]["name"] != "gemini-3-pro" {
		t.Fatalf("unexpected filtered models: %v", got)
	}

	if got := h.FilterModelsForRequest(modelAccessTestGinContext("sk-allowed"), models); len(got) != 4 {
		t.Fatalf("listed key sees %d models, want 4", len(got))
	}

	h.Cfg = modelAccessTestConfig(false)
	if got := h.FilterModelsForRequest(modelAccessTestGinContext("sk-other"), models); len(got) != 4 {
		t.Fatalf("disabled rules filtered to %d models, want 4", len(got))
	}
	var nilHandler *BaseAPIHandler
	if got := nilHandler.FilterModelsForRequest(modelAccessTestGinContext("sk-other"), models); len(got) != 4 {
		t.Fatalf("nil handler filtered to %d models, want 4", len(got))
	}
}
