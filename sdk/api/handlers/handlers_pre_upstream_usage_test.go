package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// captureModelUsagePlugin keeps only records for one model, so leftovers from
// other tests sharing the process-wide usage manager cannot leak in.
type captureModelUsagePlugin struct {
	model   string
	records chan usage.Record
}

func (p *captureModelUsagePlugin) HandleUsage(_ context.Context, record usage.Record) {
	if record.Model != p.model {
		return
	}
	select {
	case p.records <- record:
	default:
	}
}

func captureModelUsage(t *testing.T, model string) *captureModelUsagePlugin {
	t.Helper()
	plugin := &captureModelUsagePlugin{model: model, records: make(chan usage.Record, 8)}
	registerUsagePluginForTest(t, "pre-upstream-"+model, plugin)
	return plugin
}

func (p *captureModelUsagePlugin) wait(t *testing.T) usage.Record {
	t.Helper()
	select {
	case rec := <-p.records:
		return rec
	case <-time.After(2 * time.Second):
		t.Fatalf("no usage record published for model %s", p.model)
		return usage.Record{}
	}
}

func (p *captureModelUsagePlugin) assertNone(t *testing.T) {
	t.Helper()
	select {
	case rec := <-p.records:
		t.Fatalf("expected no usage record for model %s, got %+v", p.model, rec)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestUnroutableModelPublishesFailureRecord(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))

	t.Run("non-stream", func(t *testing.T) {
		const model = "pre-upstream-unroutable-a"
		capture := captureModelUsage(t, model)
		_, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai-response", model, []byte(fmt.Sprintf(`{"model":%q}`, model)), "")
		if errMsg == nil || errMsg.StatusCode != http.StatusBadRequest {
			t.Fatalf("errMsg = %+v, want 400", errMsg)
		}
		record := capture.wait(t)
		if !record.Failed || record.Fail.StatusCode != http.StatusBadRequest {
			t.Fatalf("record = failed %v status %d, want failed 400", record.Failed, record.Fail.StatusCode)
		}
		if !strings.Contains(record.Fail.Body, "unknown provider for model "+model) {
			t.Fatalf("record.Fail.Body = %q, want the unroutable-model message", record.Fail.Body)
		}
	})

	t.Run("stream", func(t *testing.T) {
		const model = "pre-upstream-unroutable-b"
		capture := captureModelUsage(t, model)
		_, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai-response", model, []byte(fmt.Sprintf(`{"model":%q}`, model)), "")
		if errMsg := <-errChan; errMsg == nil || errMsg.StatusCode != http.StatusBadRequest {
			t.Fatalf("errMsg = %+v, want 400", errMsg)
		}
		record := capture.wait(t)
		if !record.Failed || record.Fail.StatusCode != http.StatusBadRequest || !record.Stream {
			t.Fatalf("record = failed %v status %d stream %v, want failed 400 stream", record.Failed, record.Fail.StatusCode, record.Stream)
		}
	})
}

func TestNoUsableAuthPublishesFailureRecord(t *testing.T) {
	const model = "pre-upstream-no-auth"
	registry.GetGlobalRegistry().RegisterClient("pre-upstream-client", "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("pre-upstream-client") })
	capture := captureModelUsage(t, model)

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	_, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai-response", model, []byte(fmt.Sprintf(`{"model":%q}`, model)), "")
	if errMsg == nil || errMsg.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("errMsg = %+v, want 503 no auth available", errMsg)
	}
	record := capture.wait(t)
	if !record.Failed || record.Fail.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("record = failed %v status %d, want failed 503", record.Failed, record.Fail.StatusCode)
	}
	if !strings.Contains(record.Fail.Body, "no auth available") || record.Provider != "codex" {
		t.Fatalf("record = provider %q body %q, want codex and the no-auth message", record.Provider, record.Fail.Body)
	}
}

func TestPreUpstreamFailureSkippedWhenAttemptAlreadyPublished(t *testing.T) {
	const model = "pre-upstream-already-published"
	capture := captureModelUsage(t, model)
	errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: errors.New("auth_unavailable: no auth available")}

	// An upstream attempt of this request already published its own record.
	ctx, tracker := usage.WithPublishTracker(context.Background())
	usage.PublishRecord(ctx, usage.Record{Model: model, Failed: true, Fail: usage.Failure{StatusCode: http.StatusNotFound}})
	if got := capture.wait(t); got.Fail.StatusCode != http.StatusNotFound {
		t.Fatalf("attempt record status = %d, want 404", got.Fail.StatusCode)
	}
	publishPreUpstreamFailure(ctx, []string{"antigravity"}, model, tracker, errMsg, false)
	capture.assertNone(t)

	// Internal executions are not reported, matching the plugin executor path.
	publishPreUpstreamFailure(context.Background(), nil, model, nil, errMsg, true)
	capture.assertNone(t)
}
