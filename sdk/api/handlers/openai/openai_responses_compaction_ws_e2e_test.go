package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	internalutil "github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

// openAICompatWebsocketE2EUpstream answers streaming chat/completions with an SSE
// reply and non-streaming ones (the compaction summary call) with a JSON summary.
type openAICompatWebsocketE2EUpstream struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (u *openAICompatWebsocketE2EUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, errRead := io.ReadAll(r.Body)
	if errRead != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	u.mu.Lock()
	u.bodies = append(u.bodies, append([]byte(nil), body...))
	u.mu.Unlock()

	if r.URL.Path != "/v1/chat/completions" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if gjson.GetBytes(body, "stream").Bool() {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-ws\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"turn-answer\"},\"finish_reason\":null}]}\n\n"+
			"data: {\"id\":\"chatcmpl-ws\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n"+
			"data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"chatcmpl-summary","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"summary text"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
}

func (u *openAICompatWebsocketE2EUpstream) snapshot() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([][]byte, len(u.bodies))
	copy(out, u.bodies)
	return out
}

func TestOpenAICompatResponsesCompactionWebsocket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &openAICompatWebsocketE2EUpstream{}
	upstreamServer := httptest.NewServer(upstream)
	defer upstreamServer.Close()

	const model = "e2e-openai-compat-ws-compaction-model"
	compatName := "compat-ws-e2e"
	provider := internalutil.OpenAICompatibleProviderKey(compatName)
	cfg := &internalconfig.Config{
		OpenAICompatibility: []internalconfig.OpenAICompatibility{{Name: compatName, BaseURL: upstreamServer.URL + "/v1"}},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewOpenAICompatExecutor(provider, cfg))
	auth := &coreauth.Auth{
		ID:       "auth-openai-compat-ws-e2e",
		Provider: provider,
		Label:    compatName,
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"base_url":     upstreamServer.URL + "/v1",
			"api_key":      "e2e-key",
			"compat_name":  compatName,
			"provider_key": provider,
			"config_index": "0",
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()

	conn, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses/ws", nil)
	if errDial != nil {
		t.Fatalf("dial websocket: %v", errDial)
	}
	defer func() { _ = conn.Close() }()

	// Turn 1: an ordinary request builds websocket history.
	turn1 := sendOpenAICompatWebsocketE2ETurn(t, conn, fmt.Sprintf(`{"type":"response.create","model":%q,"input":[{"type":"message","role":"user","content":"first question"}]}`, model))
	if got := gjson.GetBytes(turn1, "response.output.#").Int(); got == 0 {
		t.Fatalf("turn 1 completed without output: %s", turn1)
	}

	// Turn 2: the Codex remote compaction v2 trigger must yield exactly one compaction item.
	turn2 := sendOpenAICompatWebsocketE2ETurn(t, conn, `{"type":"response.create","input":[{"type":"compaction_trigger"}]}`)
	assertOpenAICompatResponsesE2EOutput(t, []byte(gjson.GetBytes(turn2, "response").Raw))
	capsule := gjson.GetBytes(turn2, "response.output.0.encrypted_content").String()
	bodies := upstream.snapshot()
	if len(bodies) != 2 {
		t.Fatalf("upstream requests after trigger = %d, want 2", len(bodies))
	}
	summaryRequest := string(bodies[1])
	if gjson.Get(summaryRequest, "stream").Bool() {
		t.Fatalf("summary request must be non-streaming: %s", summaryRequest)
	}
	if strings.Contains(summaryRequest, "compaction_trigger") {
		t.Fatalf("trigger reached upstream: %s", summaryRequest)
	}
	if !strings.Contains(summaryRequest, "first question") || !strings.Contains(summaryRequest, "turn-answer") {
		t.Fatalf("summary request lost websocket history: %s", summaryRequest)
	}

	// Turn 3: the client replaces history with the compacted transcript. The summary must reach
	// the upstream and the pre-compaction history must not be merged back in.
	turn3 := sendOpenAICompatWebsocketE2ETurn(t, conn, fmt.Sprintf(`{"type":"response.create","input":[{"type":"message","role":"user","content":"first question"},{"type":"compaction","encrypted_content":%q},{"type":"message","role":"user","content":"next task"}]}`, capsule))
	if got := gjson.GetBytes(turn3, "response.output.#").Int(); got == 0 {
		t.Fatalf("turn 3 completed without output: %s", turn3)
	}
	bodies = upstream.snapshot()
	if len(bodies) != 3 {
		t.Fatalf("upstream requests after replay = %d, want 3", len(bodies))
	}
	replayRequest := string(bodies[2])
	if !strings.Contains(replayRequest, "summary text") {
		t.Fatalf("replay request is missing the compaction summary: %s", replayRequest)
	}
	if strings.Contains(replayRequest, capsule) || strings.Contains(replayRequest, `"type":"compaction"`) {
		t.Fatalf("opaque compaction item reached upstream: %s", replayRequest)
	}
	if strings.Contains(replayRequest, "turn-answer") {
		t.Fatalf("pre-compaction history was merged back into the replay: %s", replayRequest)
	}
	if !strings.Contains(replayRequest, "next task") {
		t.Fatalf("replay request lost the new user message: %s", replayRequest)
	}
}

// sendOpenAICompatWebsocketE2ETurn writes one response.create and returns the terminal event.
func sendOpenAICompatWebsocketE2ETurn(t *testing.T, conn *websocket.Conn, request string) []byte {
	t.Helper()
	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(request)); errWrite != nil {
		t.Fatalf("write websocket message: %v", errWrite)
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, message, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Fatalf("read websocket message: %v", errRead)
		}
		switch gjson.GetBytes(message, "type").String() {
		case "response.completed":
			return message
		case "error", "response.failed":
			t.Fatalf("websocket turn failed: %s", message)
		}
	}
}
