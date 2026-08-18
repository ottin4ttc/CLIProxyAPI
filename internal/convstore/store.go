package convstore

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// pending is an observed request waiting for its response.
type pending struct {
	createdAt     time.Time
	lastChunk     time.Time
	line          Line
	keyLabel      string
	sessionKey    string
	requestID     string
	stream        bool
	streamOpen    bool
	ended         bool // end marker seen; flushed as "ok" after streamEndGrace
	response      []byte
	respTruncated bool
}

// streamEndGrace is how long an ended stream keeps accepting chunks before
// the maintenance loop flushes it as "ok". OpenAI chat streams can carry a
// usage-only chunk after the finish_reason chunk; flushing on the marker
// alone would drop it.
const streamEndGrace = 3 * time.Second

// Error codes for turns that end as "error" without ever reaching a
// completion hook. They separate bookkeeping outcomes from real upstream
// failures, which carry the upstream error code instead.
const (
	errCodeStaleOverwritten  = "stale_overwritten"
	errCodeExpiredNoResponse = "expired_no_response"
)

// Store correlates interceptor callbacks into JSONL lines. All exported
// methods are safe for concurrent use and never block on disk I/O.
type Store struct {
	mu        sync.Mutex
	cfg       Config
	now       func() time.Time
	pendings  map[string]*pending // keyed by pluginapi RequestID
	writer    *Writer
	closeOnce sync.Once
}

// New creates a Store. now is injectable for tests; pass time.Now in main.
func New(cfg Config, now func() time.Time) *Store {
	return &Store{
		cfg:      cfg,
		now:      now,
		pendings: make(map[string]*pending),
		writer:   NewWriter(1024),
	}
}

// Reconfigure applies a new plugin config (plugin.reconfigure).
func (s *Store) Reconfigure(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

// Shutdown flushes queued writes. Open pendings are finalized as truncated.
// Safe to call more than once (tests register it in t.Cleanup and also call
// it explicitly to flush before assertions).
func (s *Store) Shutdown() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		var open []*pending
		for _, p := range s.pendings {
			open = append(open, p)
		}
		s.pendings = make(map[string]*pending)
		s.mu.Unlock()
		for _, p := range open {
			status := "truncated"
			if p.ended {
				status = "ok"
			}
			s.finalize(p, status)
		}
		s.writer.Close()
	})
}

// OnRequestBefore records an incoming client request (pre-auth hook).
func (s *Store) OnRequestBefore(req pluginapi.RequestInterceptRequest) {
	id := req.RequestID
	if id == "" {
		return
	}
	apiKey := APIKeyFromHeaders(req.Headers)
	nowTS := s.now()
	utcDate := nowTS.UTC().Format("2006-01-02")
	sessionKey, sessionID, source := ExtractSession(apiKey, req.Headers, req.Body, utcDate)

	s.mu.Lock()
	stale := s.pendings[id]
	maxBody := s.cfg.MaxBodyBytes
	body, truncated := clip(req.Body, maxBody)

	var maskedHeaders map[string]string
	if len(req.Headers) > 0 {
		maskedHeaders = make(map[string]string, len(req.Headers))
		for key, values := range req.Headers {
			if len(values) == 0 {
				continue
			}
			maskedHeaders[key] = util.MaskSensitiveHeaderValue(key, values[0])
		}
	}

	p := &pending{
		createdAt:  nowTS,
		keyLabel:   KeyLabel(apiKey),
		sessionKey: sessionKey,
		requestID:  id,
		stream:     req.Stream,
		line: Line{
			TS:             nowTS.UnixMilli(),
			TraceID:        req.TraceID,
			Model:          req.Model,
			RequestedModel: req.RequestedModel,
			Stream:         req.Stream,
			SessionID:      sessionID,
			SessionSource:  source,
			TruncatedBody:  truncated,
			Request:        RawRequest(body),
			RequestHeaders: maskedHeaders,
		},
	}
	s.pendings[id] = p
	s.mu.Unlock()
	if stale != nil {
		// The pending was replaced by a newer request carrying the same ID, so
		// it never reached a completion hook. Mark why, to keep it apart from
		// turns that failed upstream.
		stale.line.ErrorCode = errCodeStaleOverwritten
		s.finalize(stale, "error")
	}
}

// OnResponse completes a non-streaming request (response.intercept_after).
func (s *Store) OnResponse(req pluginapi.ResponseInterceptRequest) {
	s.mu.Lock()
	p := s.popPendingLocked(req.RequestID)
	if p == nil {
		s.mu.Unlock()
		return
	}
	body, truncated := clip(req.Body, s.cfg.MaxBodyBytes)
	p.response = body
	p.respTruncated = truncated
	if code, msg, found := StreamError(req.Body); found {
		p.line.ErrorCode = code
		p.line.ErrorMessage = msg
	}
	s.mu.Unlock()
	s.finalize(p, "ok")
}

// OnRequestComplete finalizes the pending for a finished request using the
// host lifecycle outcome. Requests already finalized (non-stream OnResponse
// path) are a no-op.
func (s *Store) OnRequestComplete(completion pluginapi.RequestCompletion) {
	s.mu.Lock()
	p := s.popPendingLocked(completion.RequestID)
	s.mu.Unlock()
	if p == nil {
		return
	}
	status := "ok"
	switch completion.Outcome {
	case pluginapi.RequestCompletionCanceled:
		status = "truncated"
	case pluginapi.RequestCompletionFailed, pluginapi.RequestCompletionRejected:
		status = "error"
	}
	s.finalize(p, status)
}

// popPendingLocked removes and returns the pending for id. Caller holds s.mu.
func (s *Store) popPendingLocked(id string) *pending {
	if id == "" {
		return nil
	}
	p := s.pendings[id]
	if p != nil {
		delete(s.pendings, id)
	}
	return p
}

// finalize writes the completed record as its own file under the session
// directory.
func (s *Store) finalize(p *pending, status string) {
	s.mu.Lock()
	dir := s.sessionDirLocked(p)
	s.mu.Unlock()

	if status == "ok" && (p.line.ErrorMessage != "" || p.line.ErrorCode != "") {
		status = "upstream_error"
	}
	p.line.Status = status
	p.line.FinishedAt = s.now().UnixMilli()
	p.line.Response = string(p.response)
	p.line.TruncatedBody = p.line.TruncatedBody || p.respTruncated
	raw, err := p.line.Marshal()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[conversation-store] marshal line: %v\n", err)
		return
	}
	s.writer.Write(dir, requestFileName(p.line.TS, p.requestID), raw)
}

// OnStreamChunk accumulates one downstream stream chunk
// (response.intercept_stream_chunk). The pending stays queued until an end
// marker or idle expiry finalizes it.
func (s *Store) OnStreamChunk(req pluginapi.StreamChunkInterceptRequest) {
	s.mu.Lock()
	p := s.peekStreamLocked(req.RequestID)
	if p == nil {
		s.mu.Unlock()
		return
	}
	p.lastChunk = s.now()
	if req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex {
		p.streamOpen = true
		s.mu.Unlock()
		return
	}
	if max := s.cfg.MaxBodyBytes; max <= 0 || len(p.response) < max {
		room := len(req.Body)
		if max := s.cfg.MaxBodyBytes; max > 0 && len(p.response)+room > max {
			room = max - len(p.response)
			p.respTruncated = true
		}
		p.response = append(p.response, req.Body[:room]...)
	} else {
		p.respTruncated = true
	}
	if code, msg, found := StreamError(req.Body); found && p.line.ErrorMessage == "" && p.line.ErrorCode == "" {
		// In-band upstream error (e.g. overloaded/capacity inside an HTTP-200
		// stream). Record it and let the grace flush finalize the turn.
		p.line.ErrorCode = code
		p.line.ErrorMessage = msg
		p.ended = true
	}
	if StreamEnded(req.Body) {
		// Don't finalize yet: a trailing chunk (e.g. an OpenAI usage-only
		// chunk after finish_reason) may still arrive. The maintenance loop
		// flushes ended pendings as "ok" after streamEndGrace.
		p.ended = true
	}
	s.mu.Unlock()
}

// ExpireIdle finalizes ended streams past the post-marker grace as ok,
// streams idle beyond the configured timeout as truncated, and non-stream
// pendings older than 10 minutes as errors (interceptors only fire on
// success, so a missing response means the upstream call failed).
func (s *Store) ExpireIdle() {
	const errorExpiry = 10 * time.Minute
	nowTS := s.now()
	idleLimit := time.Duration(s.cfg.StreamIdleTimeoutSeconds) * time.Second

	type expired struct {
		p      *pending
		status string
	}
	var out []expired
	s.mu.Lock()
	for id, p := range s.pendings {
		last := p.lastChunk
		if last.IsZero() {
			last = p.createdAt
		}
		switch {
		case p.stream && p.ended && nowTS.Sub(last) > streamEndGrace:
			out = append(out, expired{p, "ok"})
			delete(s.pendings, id)
		case p.stream && nowTS.Sub(last) > idleLimit:
			out = append(out, expired{p, "truncated"})
			delete(s.pendings, id)
		case !p.stream && nowTS.Sub(p.createdAt) > errorExpiry:
			p.line.ErrorCode = errCodeExpiredNoResponse
			out = append(out, expired{p, "error"})
			delete(s.pendings, id)
		}
	}
	s.mu.Unlock()
	for _, e := range out {
		s.finalize(e.p, e.status)
	}
}

// peekStreamLocked returns the streaming pending for id without removing it.
// Caller holds s.mu.
func (s *Store) peekStreamLocked(id string) *pending {
	if p := s.pendings[id]; p != nil && p.stream {
		return p
	}
	return nil
}

// sessionDirLocked builds the session directory. Caller holds s.mu.
func (s *Store) sessionDirLocked(p *pending) string {
	return filepath.Join(s.cfg.DataDir, p.keyLabel, p.sessionKey)
}

// requestFileName names one record's file. The millisecond timestamp is
// zero-padded to 13 digits so lexical order matches chronological order,
// which is what the archiver relies on when concatenating a directory.
func requestFileName(tsMillis int64, requestID string) string {
	return fmt.Sprintf("%013d-%s.jsonl", tsMillis, sanitizeName(requestID))
}

// clip bounds payload size, reporting whether it was cut.
func clip(data []byte, max int) ([]byte, bool) {
	if max > 0 && len(data) > max {
		return append([]byte(nil), data[:max]...), true
	}
	return append([]byte(nil), data...), false
}
