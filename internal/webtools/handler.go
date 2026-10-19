package webtools

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// Handler serves the /v1/tools gateway. It is a thin transport shell over
// Client: authentication happens in middleware, and every upstream status is
// passed through so callers keep the upstream's error semantics (401, 402,
// 429, 5xx) instead of a rewritten one.
type Handler struct {
	// cfg returns the live configuration so hot reload applies without
	// re-registering routes.
	cfg func() *config.Config
}

// NewHandler returns a gateway reading live configuration from cfg.
func NewHandler(cfg func() *config.Config) *Handler {
	return &Handler{cfg: cfg}
}

func (h *Handler) currentConfig() *config.Config {
	if h == nil || h.cfg == nil {
		return nil
	}
	return h.cfg()
}

// Search handles POST /v1/tools/search.
func (h *Handler) Search(c *gin.Context) {
	h.forward(c, "search", func(client *Client, body []byte) (int, []byte, error) {
		return client.Search(c.Request.Context(), body)
	})
}

// Fetch handles POST /v1/tools/fetch.
func (h *Handler) Fetch(c *gin.Context) {
	h.forward(c, "fetch", func(client *Client, body []byte) (int, []byte, error) {
		return client.Fetch(c.Request.Context(), body)
	})
}

func (h *Handler) forward(c *gin.Context, operation string, call func(*Client, []byte) (int, []byte, error)) {
	cfg := h.currentConfig()
	if cfg == nil || !cfg.WebTools.Enabled {
		writeError(c, http.StatusNotFound, "web-tools gateway is disabled")
		return
	}

	body, err := c.GetRawData()
	if err != nil {
		writeError(c, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	if !gjson.ValidBytes(body) {
		writeError(c, http.StatusBadRequest, "request body must be valid JSON")
		return
	}

	status, respBody, errCall := call(NewClient(cfg), body)
	if errCall != nil {
		var reqErr *RequestError
		if errors.As(errCall, &reqErr) {
			writeError(c, http.StatusBadRequest, reqErr.Message)
			return
		}
		// A missing key or endpoint is a deployment fault, not a caller fault.
		log.Errorf("web-tools %s: %v", operation, errCall)
		writeError(c, http.StatusServiceUnavailable, errCall.Error())
		return
	}

	c.Header("Content-Type", "application/json")
	c.Status(status)
	if len(respBody) > 0 {
		_, _ = c.Writer.Write(respBody)
	}
}

// writeError answers with the OpenAI-compatible error envelope the upstream
// Search/Fetch API uses, so clients parse one shape from either source.
func writeError(c *gin.Context, status int, message string) {
	payload, errMarshal := json.Marshal(gin.H{"error": gin.H{
		"message": message,
		"type":    errorTypeForStatus(status),
		"param":   nil,
		"code":    errorTypeForStatus(status),
	}})
	if errMarshal != nil {
		c.String(status, "%s", message)
		return
	}
	c.Header("Content-Type", "application/json")
	c.Status(status)
	_, _ = c.Writer.Write(payload)
}

func errorTypeForStatus(status int) string {
	switch {
	case status >= http.StatusInternalServerError:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}
