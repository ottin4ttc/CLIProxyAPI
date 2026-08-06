package convstore

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func streamReq(body string) pluginapi.RequestInterceptRequest {
	r := reqIntercept(body)
	r.Stream = true
	return r
}

func chunk(body, chunkData string, index int) pluginapi.StreamChunkInterceptRequest {
	return pluginapi.StreamChunkInterceptRequest{
		OriginalRequest: []byte(body),
		Body:            []byte(chunkData),
		ChunkIndex:      index,
	}
}

func TestStreamingFlushedOkAfterEndMarkerGrace(t *testing.T) {
	s, dir, clock := testStore(t)
	body := `{"messages":[{"role":"user","content":"stream"}]}`
	s.OnRequestBefore(streamReq(body))
	s.OnStreamChunk(chunk(body, "", pluginapi.StreamChunkHeaderInitIndex))
	s.OnStreamChunk(chunk(body, `{"choices":[{"delta":{"content":"a"},"finish_reason":null}]}`, 0))
	s.OnStreamChunk(chunk(body, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, 1))
	// A usage-only chunk after finish_reason must still be captured.
	s.OnStreamChunk(chunk(body, `{"choices":[],"usage":{"total_tokens":9}}`, 2))

	s.ExpireIdle() // within grace: nothing flushed yet
	if lines := readLines(t, dir); len(lines) != 0 {
		t.Fatalf("flushed before grace elapsed: %+v", lines)
	}

	*clock = clock.Add(streamEndGrace + time.Second)
	s.ExpireIdle()
	s.Shutdown()
	lines := readLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	l := lines[0]
	if l.Status != "ok" || !l.Stream {
		t.Fatalf("unexpected line: %+v", l)
	}
	want := `{"choices":[{"delta":{"content":"a"},"finish_reason":null}]}` +
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}` +
		`{"choices":[],"usage":{"total_tokens":9}}`
	if l.Response != want {
		t.Fatalf("response = %q, want %q", l.Response, want)
	}
}

func TestStreamingEndedFlushedOkOnShutdown(t *testing.T) {
	s, dir, _ := testStore(t)
	body := `{"messages":[{"role":"user","content":"stream"}]}`
	s.OnRequestBefore(streamReq(body))
	s.OnStreamChunk(chunk(body, `{"type":"message_stop"}`, 0))
	s.Shutdown()
	lines := readLines(t, dir)
	if len(lines) != 1 || lines[0].Status != "ok" {
		t.Fatalf("expected ok line on shutdown after end marker, got %+v", lines)
	}
}

func TestStreamUpstreamErrorRecorded(t *testing.T) {
	s, dir, clock := testStore(t)
	body := `{"messages":[{"role":"user","content":"stream"}]}`
	s.OnRequestBefore(streamReq(body))
	s.OnStreamChunk(chunk(body, "", pluginapi.StreamChunkHeaderInitIndex))
	s.OnStreamChunk(chunk(body, `{"type":"response.output_text.delta","delta":"a"}`, 0))
	s.OnStreamChunk(chunk(body, `{"type":"error","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`, 1))

	*clock = clock.Add(streamEndGrace + time.Second)
	s.ExpireIdle()
	s.Shutdown()
	lines := readLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	l := lines[0]
	if l.Status != "upstream_error" {
		t.Fatalf("status = %q, want upstream_error: %+v", l.Status, l)
	}
	if l.ErrorCode != "server_is_overloaded" || l.ErrorMessage != "Our servers are currently overloaded. Please try again later." {
		t.Fatalf("error fields = (%q, %q), want overloaded", l.ErrorCode, l.ErrorMessage)
	}
	if l.Response == "" {
		t.Fatalf("response chunks must still be recorded: %+v", l)
	}
}

func TestNonStreamUpstreamErrorRecorded(t *testing.T) {
	s, dir, _ := testStore(t)
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	s.OnRequestBefore(reqIntercept(body))
	s.OnResponse(pluginapi.ResponseInterceptRequest{
		OriginalRequest: []byte(body),
		Body:            []byte(`{"error":{"type":"server_error","message":"The server had an error."}}`),
		StatusCode:      200,
	})
	s.Shutdown()
	lines := readLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	l := lines[0]
	if l.Status != "upstream_error" || l.ErrorCode != "server_error" || l.ErrorMessage != "The server had an error." {
		t.Fatalf("unexpected line: %+v", l)
	}
}

func TestStreamIdleExpiry(t *testing.T) {
	s, dir, clock := testStore(t)
	body := `{"messages":[{"role":"user","content":"lost"}]}`
	s.OnRequestBefore(streamReq(body))
	s.OnStreamChunk(chunk(body, `{"choices":[{"delta":{"content":"a"},"finish_reason":null}]}`, 0))
	*clock = clock.Add(3 * time.Minute) // beyond 120s idle timeout
	s.ExpireIdle()
	s.Shutdown()
	lines := readLines(t, dir)
	if len(lines) != 1 || lines[0].Status != "truncated" {
		t.Fatalf("expected truncated line, got %+v", lines)
	}
}

func TestNonStreamErrorExpiry(t *testing.T) {
	s, dir, clock := testStore(t)
	body := `{"messages":[{"role":"user","content":"failed"}]}`
	s.OnRequestBefore(reqIntercept(body)) // non-stream, upstream will error -> no response hook
	*clock = clock.Add(11 * time.Minute)
	s.ExpireIdle()
	s.Shutdown()
	lines := readLines(t, dir)
	if len(lines) != 1 || lines[0].Status != "error" || lines[0].Response != "" {
		t.Fatalf("expected error line, got %+v", lines)
	}
}
