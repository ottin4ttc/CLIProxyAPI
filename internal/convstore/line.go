package convstore

import "encoding/json"

// Line is one recorded request turn — exactly one JSONL line.
type Line struct {
	TS int64 `json:"ts"`
	// TraceID is the CPA request ID that appears in the access log and in the
	// usage records, so a stored turn can be joined against them directly.
	TraceID        string          `json:"trace_id,omitempty"`
	Model          string          `json:"model,omitempty"`
	RequestedModel string          `json:"requested_model,omitempty"`
	Stream         bool            `json:"stream"`
	SessionID      string          `json:"session_id,omitempty"`
	SessionSource  string          `json:"session_source"`
	Status         string          `json:"status"`
	ErrorCode      string          `json:"error_code,omitempty"`
	ErrorMessage   string          `json:"error_message,omitempty"`
	FinishedAt     int64           `json:"finished_at,omitempty"`
	TruncatedBody  bool            `json:"truncated_body,omitempty"`
	Request        json.RawMessage `json:"request"`
	Response       string          `json:"response,omitempty"`
	// RequestHeaders holds the downstream request headers with sensitive
	// values masked (Authorization keeps its scheme prefix, api-key/token
	// style headers become "abcd...wxyz").
	RequestHeaders map[string]string `json:"request_headers,omitempty"`
}

// Marshal renders the line as compact JSON.
func (l Line) Marshal() ([]byte, error) {
	return json.Marshal(l)
}

// RawRequest wraps a client request body for the Request field: valid JSON
// is inlined verbatim, anything else is stored as a JSON string.
func RawRequest(body []byte) json.RawMessage {
	if json.Valid(body) {
		return json.RawMessage(body)
	}
	quoted, err := json.Marshal(string(body))
	if err != nil {
		return json.RawMessage(`""`)
	}
	return json.RawMessage(quoted)
}
