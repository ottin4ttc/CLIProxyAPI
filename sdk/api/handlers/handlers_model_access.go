package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
)

// requestAPIKey returns the authenticated client API key AuthMiddleware stored
// on the gin context, or "" when the request carries none.
func requestAPIKey(ginCtx *gin.Context) string {
	if ginCtx == nil {
		return ""
	}
	value, exists := ginCtx.Get("userApiKey")
	if !exists || value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

// modelAccessError returns a 403 when model-access rules deny the requesting
// client API key the model; nil when access is allowed or rules are disabled.
// The key is read from the gin context carried in ctx, so HTTP and WebSocket
// executions are covered by the same check.
func modelAccessError(ctx context.Context, cfg *config.SDKConfig, model string) *interfaces.ErrorMessage {
	if cfg == nil || !cfg.ModelAccess.Enabled {
		return nil
	}
	var ginCtx *gin.Context
	if ctx != nil {
		ginCtx, _ = ctx.Value("gin").(*gin.Context)
	}
	if cfg.ModelAccessAllowed(requestAPIKey(ginCtx), model) {
		return nil
	}
	return &interfaces.ErrorMessage{
		StatusCode: http.StatusForbidden,
		Error:      fmt.Errorf("model %s is not available for this API key", strings.TrimSpace(model)),
	}
}

// FilterModelsForRequest drops models the requesting client API key may not
// use under model-access rules, then drops models no credential visible to the
// key's bucket can serve under provider-access rules. It returns models
// unchanged when both rule sets are disabled, so model listings only pay for
// the filters when they are on.
func (h *BaseAPIHandler) FilterModelsForRequest(c *gin.Context, models []map[string]any) []map[string]any {
	cfg := h.CurrentConfig()
	if cfg == nil {
		return models
	}
	apiKey := requestAPIKey(c)
	modelAccessOn := cfg.ModelAccess.Enabled
	providerAccessOn := cfg.ProviderAccess.Enabled
	if !modelAccessOn && !providerAccessOn {
		return models
	}

	filtered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		if modelAccessOn && !cfg.ModelAccessAllowed(apiKey, modelListEntryID(model)) {
			continue
		}
		filtered = append(filtered, model)
	}
	if !providerAccessOn || h.AuthManager == nil || len(filtered) == 0 {
		return filtered
	}

	excludes := make(map[string][]string, len(filtered))
	ids := make([]string, 0, len(filtered))
	for _, model := range filtered {
		id := modelListEntryID(model)
		if selectors := cfg.ProviderAccessExcludeProviders(apiKey, id); len(selectors) > 0 {
			excludes[id] = selectors
		}
		ids = append(ids, id)
	}
	visible := h.AuthManager.FilterVisibleModels(ids, cfg.BucketForAPIKey(apiKey), excludes)
	out := make([]map[string]any, 0, len(filtered))
	for _, model := range filtered {
		if visible[modelListEntryID(model)] {
			out = append(out, model)
		}
	}
	return out
}

// modelListEntryID extracts the client-visible model name from a registry
// model entry: OpenAI/Claude entries carry "id", Gemini entries carry "name"
// (optionally "models/"-prefixed).
func modelListEntryID(model map[string]any) string {
	if id, _ := model["id"].(string); id != "" {
		return id
	}
	name, _ := model["name"].(string)
	return strings.TrimPrefix(name, "models/")
}
