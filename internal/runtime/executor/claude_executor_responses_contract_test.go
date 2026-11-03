package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// Observe the final HTTP body, not only the translator's intermediate output.
// These synthetic tests do not establish the deployed alias or provider behavior.
func TestClaudeResponsesFinalWireEffort(t *testing.T) {
	models := []config.PayloadModelRule{{Name: "claude-opus-5-5", Protocol: "claude"}}
	for _, effort := range []string{"max", "xhigh", "high", "medium"} {
		for _, policy := range []string{"preserve", "override", "filter"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", effort, policy, stream), func(t *testing.T) {
					cfg := &config.Config{}
					want := effort
					if policy == "override" {
						cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"output_config.effort": "low"}}}
						want = "low"
					} else if policy == "filter" {
						cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"output_config.effort"}}}
						want = ""
					}
					body, terminal := runClaudeResponsesContract(t, cfg, effort, "end_turn", stream)
					got := gjson.GetBytes(body, "output_config.effort")
					if got.String() != want || (want == "" && got.Exists()) {
						t.Fatalf("wire effort = %q (exists=%v), want %q", got.String(), got.Exists(), want)
					}
					if got := gjson.GetBytes(body, "thinking.type").String(); got != "adaptive" {
						t.Fatalf("wire thinking type = %q, want adaptive", got)
					}
					if terminal.Get("status").String() != "completed" {
						t.Fatalf("unexpected terminal status: %s", terminal.Get("status"))
					}
				})
			}
		}
	}
}

func TestClaudeResponsesSensitiveStopThroughExecutor(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			_, terminal := runClaudeResponsesContract(t, &config.Config{}, "max", "sensitive", stream)
			if terminal.Get("status").String() != "incomplete" || terminal.Get("incomplete_details.reason").String() != "content_filter" {
				t.Fatalf("sensitive stop must remain incomplete/content_filter, got %s", terminal.Raw)
			}
			if got := terminal.Get("output.0.content.0.text").String(); got != `{"picks":[` {
				t.Fatalf("partial output changed: %q", got)
			}
			if terminal.Get("usage.input_tokens").Int() != 10 || terminal.Get("usage.output_tokens").Int() != 3 {
				t.Fatalf("usage lost: %s", terminal.Get("usage"))
			}
		})
	}
}

func runClaudeResponsesContract(t *testing.T, cfg *config.Config, effort, stop string, stream bool) ([]byte, gjson.Result) {
	t.Helper()
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request: %v", errRead)
		}
		select {
		case bodies <- body:
		default:
			t.Error("unexpected upstream replay")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"message_start","message":{"id":"msg_contract","type":"message","model":"claude-opus-5-5","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"picks\":["}}`,
			`{"type":"content_block_stop","index":0}`,
			fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":3}}`, stop),
			`{"type":"message_stop"}`,
		}
		for _, event := range events {
			if _, errWrite := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", gjson.Get(event, "type").String(), event); errWrite != nil {
				t.Errorf("write response: %v", errWrite)
			}
		}
	}))
	t.Cleanup(server.Close)
	payload, errMarshal := json.Marshal(map[string]any{
		"model": "claude-opus-5-5", "stream": stream,
		"reasoning": map[string]string{"effort": effort},
		"input":     []map[string]string{{"role": "user", "content": "Synthetic contract check."}},
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	auth := &cliproxyauth.Auth{ID: "responses-contract", Attributes: map[string]string{"api_key": "test-contract-key", "base_url": server.URL}}
	req := cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: payload}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload, Stream: stream}
	exec := NewClaudeExecutor(cfg)
	var terminal gjson.Result
	if stream {
		result, errStream := exec.ExecuteStream(context.Background(), auth, req, opts)
		if errStream != nil {
			t.Fatal(errStream)
		}
		terminals := 0
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
			for _, line := range strings.Split(string(chunk.Payload), "\n") {
				if data, ok := strings.CutPrefix(line, "data: "); ok {
					event := gjson.Parse(data)
					if kind := event.Get("type").String(); kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed" {
						terminal = event.Get("response")
						terminals++
					}
				}
			}
		}
		if terminals != 1 {
			t.Fatalf("terminal events = %d, want 1", terminals)
		}
	} else {
		result, errExecute := exec.Execute(context.Background(), auth, req, opts)
		if errExecute != nil {
			t.Fatal(errExecute)
		}
		terminal = gjson.ParseBytes(result.Payload)
	}
	select {
	case body := <-bodies:
		return body, terminal
	default:
		t.Fatal("no upstream request captured")
		return nil, terminal
	}
}
