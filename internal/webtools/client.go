// Package webtools exposes a Search/Fetch API gateway so client tooling can
// borrow one upstream credential instead of holding its own key.
package webtools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// DefaultBaseURL is the upstream base used when web-tools.base-url is empty.
	DefaultBaseURL = "https://api.deepseek.com/v1/tools"

	// DefaultCount is the search result count applied when web-tools.count is
	// unset and the client omits its own count.
	DefaultCount = 10

	// defaultMinCount and defaultMaxCount bound the configured count, mirroring
	// the upstream range. Client-supplied values are forwarded unchanged so the
	// upstream reports its own validation error.
	defaultMinCount = 1
	defaultMaxCount = 50

	// maxResponseBytes caps one upstream response body. Search results carrying
	// page bodies are the largest case; anything beyond this is an upstream bug.
	maxResponseBytes = 32 << 20
)

// RequestError reports a caller-side mistake that must map to HTTP 400.
type RequestError struct {
	Message string
}

func (e *RequestError) Error() string {
	return e.Message
}

// Client forwards Search and Fetch calls to a DeepSeek-compatible web API.
// It reads its endpoint and credential per call so config hot reload applies.
type Client struct {
	cfg *config.Config
}

// NewClient returns a client reading settings from cfg on every call.
func NewClient(cfg *config.Config) *Client {
	return &Client{cfg: cfg}
}

// settings is one call's resolved configuration snapshot.
type settings struct {
	baseURL     string
	apiKey      string
	count       int
	needContent bool
}

func (c *Client) resolve() (settings, error) {
	if c == nil || c.cfg == nil {
		return settings{}, fmt.Errorf("web-tools: no configuration loaded")
	}
	gateway := c.cfg.WebTools
	out := settings{
		baseURL:     strings.TrimSpace(gateway.BaseURL),
		apiKey:      strings.TrimSpace(gateway.APIKey),
		count:       gateway.Count,
		needContent: gateway.NeedContent,
	}
	if out.baseURL == "" {
		out.baseURL = DefaultBaseURL
	}
	if out.apiKey == "" {
		out.apiKey = credentialAPIKey(c.cfg, gateway.Credential)
	}
	if out.apiKey == "" {
		return settings{}, fmt.Errorf("web-tools: no upstream API key configured")
	}
	if out.count <= 0 {
		out.count = DefaultCount
	}
	if out.count < defaultMinCount {
		out.count = defaultMinCount
	}
	if out.count > defaultMaxCount {
		out.count = defaultMaxCount
	}
	return out, nil
}

// credentialAPIKey returns the first usable key of the openai-compatibility
// entry named name. Entry names are compared case-insensitively after trimming,
// matching how the rest of the config treats provider names. A value written as
// the derived auth provider key ("openai-compatible-<name>") is also accepted,
// because that is the name the management UI and log lines show for the same
// entry.
func credentialAPIKey(cfg *config.Config, name string) string {
	name = strings.TrimSpace(name)
	if cfg == nil || name == "" {
		return ""
	}
	candidates := []string{name}
	for _, prefix := range []string{"openai-compatible-", "openai-compatibility-"} {
		if len(name) > len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
			candidates = append(candidates, strings.TrimSpace(name[len(prefix):]))
		}
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		for i := range cfg.OpenAICompatibility {
			entry := &cfg.OpenAICompatibility[i]
			if !strings.EqualFold(strings.TrimSpace(entry.Name), candidate) {
				continue
			}
			for _, key := range entry.APIKeyEntries {
				if trimmed := strings.TrimSpace(key.APIKey); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}

// Search forwards one Search API request. The returned status and body are the
// upstream's own, so callers see its result shape and error messages verbatim.
func (c *Client) Search(ctx context.Context, body []byte) (int, []byte, error) {
	resolved, err := c.resolve()
	if err != nil {
		return 0, nil, err
	}
	if strings.TrimSpace(gjson.GetBytes(body, "query").String()) == "" {
		return 0, nil, &RequestError{Message: "web-tools: search requires a non-empty \"query\""}
	}
	return c.post(ctx, resolved, "/search", applySearchDefaults(body, resolved))
}

// Fetch forwards one Fetch API request.
func (c *Client) Fetch(ctx context.Context, body []byte) (int, []byte, error) {
	resolved, err := c.resolve()
	if err != nil {
		return 0, nil, err
	}
	if strings.TrimSpace(gjson.GetBytes(body, "url").String()) == "" {
		return 0, nil, &RequestError{Message: "web-tools: fetch requires a non-empty \"url\""}
	}
	return c.post(ctx, resolved, "/fetch", body)
}

// applySearchDefaults fills the configured count and need_content only when the
// client left the field out, so explicit client values always win.
func applySearchDefaults(body []byte, resolved settings) []byte {
	if !gjson.GetBytes(body, "count").Exists() {
		body, _ = sjson.SetBytes(body, "count", resolved.count)
	}
	if resolved.needContent && !gjson.GetBytes(body, "need_content").Exists() {
		body, _ = sjson.SetBytes(body, "need_content", true)
	}
	return body
}

func (c *Client) post(ctx context.Context, resolved settings, path string, body []byte) (int, []byte, error) {
	endpoint := strings.TrimSuffix(resolved.baseURL, "/") + path
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("web-tools: build %s request: %w", path, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+resolved.apiKey)

	client := helps.NewProxyAwareHTTPClient(ctx, c.cfg, nil, 0)
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return 0, nil, fmt.Errorf("web-tools: %s request failed: %w", path, err)
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			_ = errClose
		}
	}()

	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("web-tools: read %s response: %w", path, err)
	}
	return httpResp.StatusCode, respBody, nil
}
