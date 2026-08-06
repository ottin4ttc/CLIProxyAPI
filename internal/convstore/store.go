package convstore

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// pending is an observed request waiting for its response.
type pending struct {
	createdAt     time.Time
	lastChunk     time.Time
	line          Line
	keyLabel      string
	sessionKey    string
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

// Store correlates interceptor callbacks into JSONL lines. All exported
// methods are safe for concurrent use and never block on disk I/O.
type Store struct {
	mu        sync.Mutex
	cfg       Config
	now       func() time.Time
	pendings  map[[32]byte][]*pending
	turns     map[string]int // session file path -> last written turn
	writer    *Writer
	closeOnce sync.Once
}

// New creates a Store. now is injectable for tests; pass time.Now in main.
func New(cfg Config, now func() time.Time) *Store {
	return &Store{
		cfg:      cfg,
		now:      now,
		pendings: make(map[[32]byte][]*pending),
		turns:    make(map[string]int),
		writer:   NewWriter(1024),
	}
}

// Reconfigure applies a new plugin config (plugin.reconfigure).
func (s *Store) Reconfigure(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

// Shutdown flushes queued appends. Open pendings are finalized as truncated.
// Safe to call more than once (tests register it in t.Cleanup and also call
// it explicitly to flush before assertions).
func (s *Store) Shutdown() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		var open []*pending
		for _, queue := range s.pendings {
			open = append(open, queue...)
		}
		s.pendings = make(map[[32]byte][]*pending)
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
	apiKey := APIKeyFromHeaders(req.Headers)
	nowTS := s.now()
	utcDate := nowTS.UTC().Format("2006-01-02")
	sessionKey, sessionID, source := ExtractSession(apiKey, req.Headers, req.Body, utcDate)

	s.mu.Lock()
	defer s.mu.Unlock()
	maxBody := s.cfg.MaxBodyBytes
	body, truncated := clip(req.Body, maxBody)
	p := &pending{
		createdAt:  nowTS,
		keyLabel:   KeyLabel(apiKey),
		sessionKey: sessionKey,
		stream:     req.Stream,
		line: Line{
			TS:             nowTS.UnixMilli(),
			Model:          req.Model,
			RequestedModel: req.RequestedModel,
			Stream:         req.Stream,
			SessionID:      sessionID,
			SessionSource:  source,
			TruncatedBody:  truncated,
			Request:        RawRequest(body),
		},
	}
	hash := sha256.Sum256(req.Body)
	s.pendings[hash] = append(s.pendings[hash], p)
}

// OnResponse completes a non-streaming request (response.intercept_after).
func (s *Store) OnResponse(req pluginapi.ResponseInterceptRequest) {
	hash := sha256.Sum256(req.OriginalRequest)
	s.mu.Lock()
	p := s.popPendingLocked(hash)
	if p == nil {
		s.mu.Unlock()
		fmt.Fprintln(os.Stderr, "[conversation-store] response without pending request, dropped")
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

// popPendingLocked removes and returns the oldest pending for hash.
// Caller holds s.mu.
func (s *Store) popPendingLocked(hash [32]byte) *pending {
	queue := s.pendings[hash]
	if len(queue) == 0 {
		return nil
	}
	p := queue[0]
	if len(queue) == 1 {
		delete(s.pendings, hash)
	} else {
		s.pendings[hash] = queue[1:]
	}
	return p
}

// finalize assigns the turn number and enqueues the completed line.
//
// Turn numbers are unique and strictly monotonic per session file, but the
// seeding I/O below and the writer's actual disk append both happen outside
// s.mu, so under concurrent finalizes for the same file the physical line
// order on disk is not guaranteed to match turn order. Consumers must order
// lines by the "turn" field, not by file position.
func (s *Store) finalize(p *pending, status string) {
	s.mu.Lock()
	path := s.sessionPathLocked(p)
	turn, seeded := s.turns[path]
	s.mu.Unlock()

	if !seeded {
		// Seeding (RecoverTornLine + CountLines) does synchronous disk I/O.
		// Run it outside s.mu so a session file's first touch never blocks
		// OnRequestBefore/OnResponse for every other session. Two
		// goroutines may race to seed the same brand-new path here; both
		// operations are read-only plus a tail-truncate, so running them
		// twice concurrently is harmless.
		if err := RecoverTornLine(path); err != nil {
			fmt.Fprintf(os.Stderr, "[conversation-store] recover %s: %v\n", path, err)
		}
		n, err := CountLines(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[conversation-store] count lines %s: %v\n", path, err)
		} else {
			turn = n
		}
	}

	s.mu.Lock()
	if seededTurn, ok := s.turns[path]; ok {
		// Another goroutine seeded (and possibly advanced) this path while
		// we were doing unlocked I/O; our count is stale, so use the
		// authoritative map value instead.
		turn = seededTurn
	}
	turn++
	s.turns[path] = turn
	s.mu.Unlock()

	if status == "ok" && (p.line.ErrorMessage != "" || p.line.ErrorCode != "") {
		status = "upstream_error"
	}
	p.line.Turn = turn
	p.line.Status = status
	p.line.FinishedAt = s.now().UnixMilli()
	p.line.Response = string(p.response)
	p.line.TruncatedBody = p.line.TruncatedBody || p.respTruncated
	raw, err := p.line.Marshal()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[conversation-store] marshal line: %v\n", err)
		return
	}
	s.writer.Append(path, raw)
}

// OnStreamChunk accumulates one downstream stream chunk
// (response.intercept_stream_chunk). The pending stays queued until an end
// marker or idle expiry finalizes it.
func (s *Store) OnStreamChunk(req pluginapi.StreamChunkInterceptRequest) {
	hash := sha256.Sum256(req.OriginalRequest)
	s.mu.Lock()
	p := s.peekStreamLocked(hash)
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
	for hash, queue := range s.pendings {
		kept := queue[:0]
		for _, p := range queue {
			last := p.lastChunk
			if last.IsZero() {
				last = p.createdAt
			}
			switch {
			case p.stream && p.ended && nowTS.Sub(last) > streamEndGrace:
				out = append(out, expired{p, "ok"})
			case p.stream && nowTS.Sub(last) > idleLimit:
				out = append(out, expired{p, "truncated"})
			case !p.stream && nowTS.Sub(p.createdAt) > errorExpiry:
				out = append(out, expired{p, "error"})
			default:
				kept = append(kept, p)
			}
		}
		if len(kept) == 0 {
			delete(s.pendings, hash)
		} else {
			s.pendings[hash] = kept
		}
	}
	s.mu.Unlock()
	for _, e := range out {
		s.finalize(e.p, e.status)
	}
}

// peekStreamLocked returns the oldest streaming pending for hash without
// removing it. Caller holds s.mu.
func (s *Store) peekStreamLocked(hash [32]byte) *pending {
	for _, p := range s.pendings[hash] {
		if p.stream {
			return p
		}
	}
	return nil
}

// sessionPathLocked builds the target file path. Caller holds s.mu.
func (s *Store) sessionPathLocked(p *pending) string {
	return filepath.Join(s.cfg.DataDir, p.keyLabel, p.sessionKey+".jsonl")
}

// clip bounds payload size, reporting whether it was cut.
func clip(data []byte, max int) ([]byte, bool) {
	if max > 0 && len(data) > max {
		return append([]byte(nil), data[:max]...), true
	}
	return append([]byte(nil), data...), false
}
