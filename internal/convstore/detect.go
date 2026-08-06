package convstore

import (
	"bytes"

	"github.com/tidwall/gjson"
)

// streamEndMarkers detect end-of-stream across the client-facing formats
// CLIProxyAPI serves. The host passes chunk payloads as bare JSON events —
// SSE framing ("data: ...", "event: ...") is added by the HTTP writer after
// interceptors run, so the plugin never sees "data: [DONE]". The framed
// variants are kept as a safety net in case some path forwards raw SSE.
// Matching is a heuristic (a marker could in principle appear inside quoted
// content); the stream idle timeout is the correctness backstop.
var streamEndMarkers = [][]byte{
	// Bare JSON events (what interceptors actually receive).
	[]byte(`"finish_reason":"`),     // OpenAI chat completions: non-null finish_reason
	[]byte(`"finish_reason": "`),    // (every chunk carries "finish_reason":null, so match only a set value)
	[]byte(`"type":"message_stop"`), // Claude messages
	[]byte(`"type": "message_stop"`),
	[]byte(`"type":"response.completed"`), // OpenAI Responses
	[]byte(`"type": "response.completed"`),
	[]byte(`"finishReason"`), // Gemini generateContent
	// SSE-framed variants (defensive; not observed from the host).
	[]byte("data: [DONE]"),
	[]byte("event: message_stop"),
	[]byte("event: response.completed"),
}

// StreamEnded reports whether a downstream chunk contains a known
// end-of-stream marker for any supported protocol.
func StreamEnded(chunk []byte) bool {
	if len(chunk) == 0 {
		return false
	}
	for _, marker := range streamEndMarkers {
		if bytes.Contains(chunk, marker) {
			return true
		}
	}
	return false
}

// StreamError detects an in-band upstream error in a chunk or response body.
// Some upstreams (e.g. the ChatGPT/Codex backend) report failures like
// "server is overloaded" or "model is at capacity" as events inside an
// otherwise successful HTTP-200 stream; the proxy forwards them as ordinary
// payloads, so this is the only place they can be observed. Recognized shapes:
//
//	{"type":"error", "error":{...}} or {"type":"error","code":...,"message":...}
//	{"type":"response.failed","response":{"error":{...}}}
//	{"error":{"message":...}}  (bare error object, non-stream bodies)
//
// The code falls back to the error object's "type" when it carries no "code"
// (Claude-style errors).
//
// Chunks are usually bare JSON events, but the openai-responses protocol
// delivers them SSE-framed ("event: ...\ndata: {...}"); non-JSON chunks are
// deframed and each data payload checked in turn.
func StreamError(chunk []byte) (code, message string, found bool) {
	if len(chunk) == 0 {
		return "", "", false
	}
	if gjson.ValidBytes(chunk) {
		return streamErrorEvent(chunk)
	}
	for _, line := range bytes.Split(chunk, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if !gjson.ValidBytes(payload) {
			continue
		}
		if code, message, found = streamErrorEvent(payload); found {
			return code, message, true
		}
	}
	return "", "", false
}

// streamErrorEvent inspects a single bare JSON event for an upstream error.
func streamErrorEvent(chunk []byte) (code, message string, found bool) {
	root := gjson.ParseBytes(chunk)
	extract := func(obj gjson.Result) (string, string) {
		c := obj.Get("code").String()
		if c == "" {
			c = obj.Get("type").String()
		}
		return c, obj.Get("message").String()
	}
	switch root.Get("type").String() {
	case "error":
		if errObj := root.Get("error"); errObj.IsObject() {
			code, message = extract(errObj)
		} else {
			code, message = extract(root)
		}
		return code, message, true
	case "response.failed":
		errObj := root.Get("response.error")
		if !errObj.IsObject() {
			errObj = root.Get("error")
		}
		if errObj.IsObject() {
			code, message = extract(errObj)
		}
		return code, message, true
	case "":
		// Bare error object: require a message to avoid matching unrelated
		// payloads that merely carry an "error" key.
		if errObj := root.Get("error"); errObj.IsObject() {
			code, message = extract(errObj)
			if message != "" {
				return code, message, true
			}
		}
	}
	return "", "", false
}
