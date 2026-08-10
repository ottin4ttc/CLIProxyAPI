package convstore

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestStreamingConversationRecordedEndToEnd drives the Hook exactly the way
// sdk/api/handlers does for one streaming turn and asserts the JSONL output.
func TestStreamingConversationRecordedEndToEnd(t *testing.T) {
	dir := t.TempDir()
	st := New(Config{DataDir: dir, MaxBodyBytes: 1 << 20, StreamIdleTimeoutSeconds: 120}, timeNowForTests(t))
	h := NewHook(st, nil)
	ctx := context.Background()

	headers := http.Header{"Authorization": []string{"Bearer sk-test-1234567890abcdef"}}
	reqBody := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`)

	h.InterceptRequestBeforeAuth(ctx, pluginapi.RequestInterceptRequest{
		RequestID: "e2e-1", Headers: headers, Body: reqBody, Stream: true, Model: "gpt-5.5",
	})
	// header-init call, as handlers_stream.go:88 issues it
	h.InterceptStreamChunk(ctx, pluginapi.StreamChunkInterceptRequest{
		RequestID: "e2e-1", ChunkIndex: pluginapi.StreamChunkHeaderInitIndex,
	})
	h.InterceptStreamChunk(ctx, pluginapi.StreamChunkInterceptRequest{
		RequestID: "e2e-1", ChunkIndex: 0, Body: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"),
	})
	h.InterceptStreamChunk(ctx, pluginapi.StreamChunkInterceptRequest{
		RequestID: "e2e-1", ChunkIndex: 1, Body: []byte("data: [DONE]\n\n"),
	})
	h.CompleteRequest(ctx, pluginapi.RequestCompletion{
		RequestID: "e2e-1", Outcome: pluginapi.RequestCompletionSucceeded,
	})
	st.Shutdown()

	var lines []map[string]any
	errWalk := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(path) != ".jsonl" {
			return err
		}
		data, errRead := os.ReadFile(path)
		if errRead != nil {
			return errRead
		}
		// Split JSONL by newlines before unmarshaling
		for _, raw := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if raw == "" {
				continue
			}
			var line map[string]any
			if errJSON := json.Unmarshal([]byte(raw), &line); errJSON != nil {
				return errJSON
			}
			lines = append(lines, line)
		}
		return nil
	})
	if errWalk != nil {
		t.Fatalf("walk: %v", errWalk)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 recorded turn, got %d", len(lines))
	}
	// Field names must match internal/convstore/line.go JSON tags — verify
	// tags before finalizing assertions.
	if lines[0]["status"] != "ok" {
		t.Fatalf("status = %v, want ok", lines[0]["status"])
	}
	if _, hasTurn := lines[0]["turn"]; hasTurn {
		t.Fatal("turn field should no longer be recorded")
	}
	if _, hasHeaders := lines[0]["request_headers"]; !hasHeaders {
		t.Fatal("request_headers missing from recorded line")
	}
}
