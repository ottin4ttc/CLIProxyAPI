package convstore

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// innerInterceptorHost mirrors handlers.PluginInterceptorHost plus the
// optional structural interfaces the handlers probe. *pluginhost.Host
// implements all of them.
type innerInterceptorHost interface {
	InterceptRequestBeforeAuth(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse
	InterceptRequestAfterAuth(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse
	InterceptResponse(context.Context, pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse
	InterceptStreamChunk(context.Context, pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse
}

type innerSkipHost interface {
	InterceptRequestBeforeAuthExcept(context.Context, pluginapi.RequestInterceptRequest, string) pluginapi.RequestInterceptResponse
	InterceptRequestAfterAuthExcept(context.Context, pluginapi.RequestInterceptRequest, string) pluginapi.RequestInterceptResponse
	InterceptResponseExcept(context.Context, pluginapi.ResponseInterceptRequest, string) pluginapi.ResponseInterceptResponse
	InterceptStreamChunkExcept(context.Context, pluginapi.StreamChunkInterceptRequest, string) pluginapi.StreamChunkInterceptResponse
}

type innerDetectors interface {
	HasStreamInterceptors() bool
	HasRequestInterceptors() bool
}

type innerLifecycle interface {
	CompleteRequest(context.Context, pluginapi.RequestCompletion)
}

type innerLifecycleSkip interface {
	CompleteRequestExcept(context.Context, pluginapi.RequestCompletion, string)
}

// Hook records conversations natively while forwarding every interceptor
// call to the wrapped plugin host (which may be absent).
type Hook struct {
	store    *Store
	inner    innerInterceptorHost
	skip     innerSkipHost
	detect   innerDetectors
	life     innerLifecycle
	lifeSkip innerLifecycleSkip
}

// NewHook wraps inner (the real plugin host, may be nil) with native
// conversation recording backed by st.
func NewHook(st *Store, inner any) *Hook {
	h := &Hook{store: st}
	if inner == nil {
		return h
	}
	h.inner, _ = inner.(innerInterceptorHost)
	h.skip, _ = inner.(innerSkipHost)
	h.detect, _ = inner.(innerDetectors)
	h.life, _ = inner.(innerLifecycle)
	h.lifeSkip, _ = inner.(innerLifecycleSkip)
	return h
}

func (h *Hook) InterceptRequestBeforeAuth(ctx context.Context, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	h.store.OnRequestBefore(req)
	if h.inner == nil {
		return pluginapi.RequestInterceptResponse{}
	}
	return h.inner.InterceptRequestBeforeAuth(ctx, req)
}

func (h *Hook) InterceptRequestBeforeAuthExcept(ctx context.Context, req pluginapi.RequestInterceptRequest, skipPluginID string) pluginapi.RequestInterceptResponse {
	h.store.OnRequestBefore(req)
	if h.skip != nil {
		return h.skip.InterceptRequestBeforeAuthExcept(ctx, req, skipPluginID)
	}
	if h.inner != nil {
		return h.inner.InterceptRequestBeforeAuth(ctx, req)
	}
	return pluginapi.RequestInterceptResponse{}
}

func (h *Hook) InterceptRequestAfterAuth(ctx context.Context, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	if h.inner == nil {
		return pluginapi.RequestInterceptResponse{}
	}
	return h.inner.InterceptRequestAfterAuth(ctx, req)
}

func (h *Hook) InterceptRequestAfterAuthExcept(ctx context.Context, req pluginapi.RequestInterceptRequest, skipPluginID string) pluginapi.RequestInterceptResponse {
	if h.skip != nil {
		return h.skip.InterceptRequestAfterAuthExcept(ctx, req, skipPluginID)
	}
	if h.inner != nil {
		return h.inner.InterceptRequestAfterAuth(ctx, req)
	}
	return pluginapi.RequestInterceptResponse{}
}

func (h *Hook) InterceptResponse(ctx context.Context, req pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
	var resp pluginapi.ResponseInterceptResponse
	if h.inner != nil {
		resp = h.inner.InterceptResponse(ctx, req)
	}
	record := req
	if len(resp.Body) > 0 {
		record.Body = resp.Body
	}
	h.store.OnResponse(record)
	return resp
}

func (h *Hook) InterceptResponseExcept(ctx context.Context, req pluginapi.ResponseInterceptRequest, skipPluginID string) pluginapi.ResponseInterceptResponse {
	var resp pluginapi.ResponseInterceptResponse
	if h.skip != nil {
		resp = h.skip.InterceptResponseExcept(ctx, req, skipPluginID)
	} else if h.inner != nil {
		resp = h.inner.InterceptResponse(ctx, req)
	}
	record := req
	if len(resp.Body) > 0 {
		record.Body = resp.Body
	}
	h.store.OnResponse(record)
	return resp
}

func (h *Hook) InterceptStreamChunk(ctx context.Context, req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	var resp pluginapi.StreamChunkInterceptResponse
	if h.inner != nil {
		resp = h.inner.InterceptStreamChunk(ctx, req)
	}
	h.recordChunk(req, resp)
	return resp
}

func (h *Hook) InterceptStreamChunkExcept(ctx context.Context, req pluginapi.StreamChunkInterceptRequest, skipPluginID string) pluginapi.StreamChunkInterceptResponse {
	var resp pluginapi.StreamChunkInterceptResponse
	if h.skip != nil {
		resp = h.skip.InterceptStreamChunkExcept(ctx, req, skipPluginID)
	} else if h.inner != nil {
		resp = h.inner.InterceptStreamChunk(ctx, req)
	}
	h.recordChunk(req, resp)
	return resp
}

// recordChunk feeds the store what the client actually receives: the inner
// plugin's replacement body when present, nothing when the chunk is dropped.
func (h *Hook) recordChunk(req pluginapi.StreamChunkInterceptRequest, resp pluginapi.StreamChunkInterceptResponse) {
	if resp.DropChunk {
		return
	}
	record := req
	if len(resp.Body) > 0 {
		record.Body = resp.Body
	}
	h.store.OnStreamChunk(record)
}

func (h *Hook) HasStreamInterceptors() bool {
	return true
}

func (h *Hook) HasRequestInterceptors() bool {
	return true
}

func (h *Hook) CompleteRequest(ctx context.Context, completion pluginapi.RequestCompletion) {
	if h.life != nil {
		h.life.CompleteRequest(ctx, completion)
	}
	h.store.OnRequestComplete(completion)
}

func (h *Hook) CompleteRequestExcept(ctx context.Context, completion pluginapi.RequestCompletion, skipPluginID string) {
	if h.lifeSkip != nil {
		h.lifeSkip.CompleteRequestExcept(ctx, completion, skipPluginID)
	} else if h.life != nil {
		h.life.CompleteRequest(ctx, completion)
	}
	h.store.OnRequestComplete(completion)
}
