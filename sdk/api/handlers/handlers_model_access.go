package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
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
// use under model-access rules. It returns models unchanged when rules are
// disabled, so model listings only pay for the filter when it is on.
func (h *BaseAPIHandler) FilterModelsForRequest(c *gin.Context, models []map[string]any) []map[string]any {
	if h == nil || h.Cfg == nil || !h.Cfg.ModelAccess.Enabled {
		return models
	}
	apiKey := requestAPIKey(c)
	filtered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		if h.Cfg.ModelAccessAllowed(apiKey, modelListEntryID(model)) {
			filtered = append(filtered, model)
		}
	}
	return filtered
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
