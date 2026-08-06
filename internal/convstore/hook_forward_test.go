package convstore

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type fakeInner struct {
	beforeCalls int
	chunkCalls  int
}

func (f *fakeInner) InterceptRequestBeforeAuth(_ context.Context, _ pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	f.beforeCalls++
	return pluginapi.RequestInterceptResponse{}
}
func (f *fakeInner) InterceptRequestAfterAuth(_ context.Context, _ pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{}
}
func (f *fakeInner) InterceptResponse(_ context.Context, _ pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
	return pluginapi.ResponseInterceptResponse{}
}
func (f *fakeInner) InterceptStreamChunk(_ context.Context, _ pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	f.chunkCalls++
	return pluginapi.StreamChunkInterceptResponse{}
}

func timeNowForTests(t *testing.T) func() time.Time {
	t.Helper()
	base := time.Unix(1700000000, 0)
	return func() time.Time { return base }
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st := New(Config{DataDir: t.TempDir(), MaxBodyBytes: 1 << 20, StreamIdleTimeoutSeconds: 120}, timeNowForTests(t))
	t.Cleanup(st.Shutdown)
	return st
}

// assertSingleLineStatus reads the single recorded JSONL line for st and
// asserts its "status" field equals want.
func assertSingleLineStatus(t *testing.T, st *Store, want string) {
	t.Helper()
	line := readSingleLine(t, st)
	if got := line["status"]; got != want {
		t.Fatalf("status = %v, want %q (line: %+v)", got, want, line)
	}
}

func TestHookForwardsToInner(t *testing.T) {
	inner := &fakeInner{}
	h := NewHook(newTestStore(t), inner)
	h.InterceptRequestBeforeAuth(context.Background(), pluginapi.RequestInterceptRequest{RequestID: "r1"})
	h.InterceptStreamChunk(context.Background(), pluginapi.StreamChunkInterceptRequest{RequestID: "r1", Body: []byte("x")})
	if inner.beforeCalls != 1 || inner.chunkCalls != 1 {
		t.Fatalf("inner not forwarded: before=%d chunk=%d", inner.beforeCalls, inner.chunkCalls)
	}
}

func TestHookNilInnerDoesNotPanic(t *testing.T) {
	h := NewHook(newTestStore(t), nil)
	h.InterceptRequestBeforeAuth(context.Background(), pluginapi.RequestInterceptRequest{RequestID: "r1"})
	h.InterceptStreamChunk(context.Background(), pluginapi.StreamChunkInterceptRequest{RequestID: "r1", Body: []byte("x")})
	h.CompleteRequest(context.Background(), pluginapi.RequestCompletion{RequestID: "r1"})
	if !h.HasStreamInterceptors() || !h.HasRequestInterceptors() {
		t.Fatal("detectors must report true so handlers feed the hook")
	}
}

func TestCompleteFinalizesStream(t *testing.T) {
	st := newTestStore(t)
	h := NewHook(st, nil)
	ctx := context.Background()
	h.InterceptRequestBeforeAuth(ctx, pluginapi.RequestInterceptRequest{
		RequestID: "r1", Stream: true, Body: []byte(`{"model":"m"}`),
	})
	h.InterceptStreamChunk(ctx, pluginapi.StreamChunkInterceptRequest{RequestID: "r1", ChunkIndex: 0, Body: []byte("data: hello\n\n")})
	h.CompleteRequest(ctx, pluginapi.RequestCompletion{RequestID: "r1", Outcome: pluginapi.RequestCompletionSucceeded})
	st.Shutdown() // flush writer
	assertSingleLineStatus(t, st, "ok")
}

func TestCompleteRejectedFinalizesAsError(t *testing.T) {
	st := newTestStore(t)
	h := NewHook(st, nil)
	ctx := context.Background()
	h.InterceptRequestBeforeAuth(ctx, pluginapi.RequestInterceptRequest{
		RequestID: "r2", Body: []byte(`{"model":"m"}`),
	})
	h.CompleteRequest(ctx, pluginapi.RequestCompletion{RequestID: "r2", Outcome: pluginapi.RequestCompletionRejected})
	st.Shutdown() // flush writer
	assertSingleLineStatus(t, st, "error")
}
