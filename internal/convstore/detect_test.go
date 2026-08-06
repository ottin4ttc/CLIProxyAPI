package convstore

import "testing"

func TestStreamEnded(t *testing.T) {
	cases := []struct {
		name  string
		chunk string
		want  bool
	}{
		// Bare JSON events, as the host actually delivers them.
		{"openai-finish-reason", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, true},
		{"openai-finish-reason-spaced", `{"choices": [{"delta": {}, "finish_reason": "length"}]}`, true},
		{"openai-usage-only", `{"choices":[],"usage":{"total_tokens":9}}`, false},
		{"openai-null-finish-reason", `{"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`, false},
		{"claude-message-stop", `{"type":"message_stop"}`, true},
		{"claude-message-stop-spaced", `{"type": "message_stop"}`, true},
		{"claude-mid-stream", `{"type":"content_block_delta","delta":{"text":"hi"}}`, false},
		{"responses-completed", `{"type":"response.completed","response":{}}`, true},
		{"responses-mid-stream", `{"type":"response.output_text.delta","delta":"hi"}`, false},
		{"gemini-finish", `{"candidates":[{"finishReason":"STOP"}]}`, true},
		// SSE-framed variants kept as a safety net.
		{"framed-openai-done", "data: [DONE]\n\n", true},
		{"framed-claude-stop", "event: message_stop\ndata: {}\n\n", true},
		{"framed-responses-completed", "event: response.completed\ndata: {}\n\n", true},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := StreamEnded([]byte(c.chunk)); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestStreamError(t *testing.T) {
	cases := []struct {
		name     string
		chunk    string
		wantCode string
		wantMsg  string
		want     bool
	}{
		// OpenAI Responses / Codex in-band error events.
		{
			"responses-error-nested",
			`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`,
			"server_is_overloaded", "Our servers are currently overloaded. Please try again later.", true,
		},
		{
			"responses-error-flat",
			`{"type":"error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}`,
			"server_is_overloaded", "Our servers are currently overloaded. Please try again later.", true,
		},
		{
			"responses-failed",
			`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"model_capacity_exceeded","message":"Selected model is at capacity. Please try a different model."}}}`,
			"model_capacity_exceeded", "Selected model is at capacity. Please try a different model.", true,
		},
		{
			"responses-failed-no-detail",
			`{"type":"response.failed","response":{"id":"resp_1","status":"failed"}}`,
			"", "", true,
		},
		// Claude messages error event (code falls back to error.type).
		{
			"claude-error",
			`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			"overloaded_error", "Overloaded", true,
		},
		// Bare error object (non-stream bodies, chat-completions style).
		{
			"bare-error-object",
			`{"error":{"type":"server_error","message":"The server had an error."}}`,
			"server_error", "The server had an error.", true,
		},
		// SSE-framed events: the openai-responses protocol delivers chunks
		// with framing intact ("event: ...\ndata: {...}"), observed live on
		// /v1/responses passthrough. Detection must deframe.
		{
			"framed-error",
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"service_unavailable_error\",\"code\":\"server_is_overloaded\",\"message\":\"Our servers are currently overloaded. Please try again later.\"}}\n\n",
			"server_is_overloaded", "Our servers are currently overloaded. Please try again later.", true,
		},
		{
			"framed-response-failed",
			"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Our servers are currently overloaded. Please try again later.\"}}}\n\n",
			"server_is_overloaded", "Our servers are currently overloaded. Please try again later.", true,
		},
		{
			"framed-multi-event",
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Our servers are currently overloaded. Please try again later.\"}}\n\n",
			"server_is_overloaded", "Our servers are currently overloaded. Please try again later.", true,
		},
		{
			"framed-completed",
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n",
			"", "", false,
		},
		// Negatives.
		{"responses-completed", `{"type":"response.completed","response":{}}`, "", "", false},
		{"responses-delta", `{"type":"response.output_text.delta","delta":"hi"}`, "", "", false},
		{"chat-chunk", `{"choices":[{"delta":{"content":"a"},"finish_reason":null}]}`, "", "", false},
		{"error-word-in-content", `{"type":"content_block_delta","delta":{"text":"error: foo"}}`, "", "", false},
		{"error-string-field", `{"choices":[{"delta":{"content":"x"}}],"error":""}`, "", "", false},
		{"empty", "", "", "", false},
		{"non-json", "data: [DONE]", "", "", false},
	}
	for _, c := range cases {
		code, msg, found := StreamError([]byte(c.chunk))
		if found != c.want || code != c.wantCode || msg != c.wantMsg {
			t.Errorf("%s: got (%q, %q, %v) want (%q, %q, %v)", c.name, code, msg, found, c.wantCode, c.wantMsg, c.want)
		}
	}
}
