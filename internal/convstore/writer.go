package convstore

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type appendRequest struct {
	path string
	line []byte
}

// Writer serializes all file appends through one goroutine so interleaved
// sessions never contend and interceptor hooks never block on disk I/O.
type Writer struct {
	mu      sync.Mutex
	closed  bool
	queue   chan appendRequest
	done    chan struct{}
	pending map[string]int // path -> enqueued-but-not-yet-written append count
}

// NewWriter starts the background append goroutine.
func NewWriter(queueSize int) *Writer {
	w := &Writer{queue: make(chan appendRequest, queueSize), done: make(chan struct{}), pending: make(map[string]int)}
	go w.run()
	return w
}

func (w *Writer) run() {
	defer close(w.done)
	for req := range w.queue {
		if err := appendLine(req.path, req.line); err != nil {
			fmt.Fprintf(os.Stderr, "[conversation-store] append %s: %v\n", req.path, err)
		}
		w.decrementPending(req.path)
	}
}

// decrementPending marks one queued append for path as written, dropping the
// entry once its count reaches zero.
func (w *Writer) decrementPending(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n, ok := w.pending[path]; ok {
		if n <= 1 {
			delete(w.pending, path)
		} else {
			w.pending[path] = n - 1
		}
	}
}

// HasPending reports whether path has an append enqueued but not yet written
// to disk. Maintenance uses this to avoid archiving or forgetting turn
// counters for a file the writer goroutine is about to (re)create.
func (w *Writer) HasPending(path string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pending[path] > 0
}

// Append enqueues one line for req.path. It never blocks: when the queue is
// full the record is dropped and logged, recording must not stall proxying.
// A copy of line is queued so the caller's backing array is never mutated
// or raced against by the writer goroutine. Appending after Close silently
// drops the record and logs to stderr; recording must never break proxying.
func (w *Writer) Append(path string, line []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		fmt.Fprintf(os.Stderr, "[conversation-store] writer closed, dropping record for %s\n", path)
		return
	}
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')
	select {
	case w.queue <- appendRequest{path: path, line: buf}:
		w.pending[path]++
	default:
		fmt.Fprintf(os.Stderr, "[conversation-store] queue full, dropping record for %s\n", path)
	}
}

// Close drains pending appends and stops the goroutine. It is safe to call
// concurrently with Append and safe to call more than once.
func (w *Writer) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		<-w.done
		return
	}
	w.closed = true
	close(w.queue)
	w.mu.Unlock()
	<-w.done
}

func appendLine(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	_, errWrite := f.Write(line)
	if errClose := f.Close(); errWrite == nil {
		errWrite = errClose
	}
	return errWrite
}

// RecoverTornLine truncates a trailing partial line left by a crash.
// A missing file is not an error.
func RecoverTornLine(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return nil
	}
	cut := bytes.LastIndexByte(data, '\n') + 1
	return os.Truncate(path, int64(cut))
}

// CountLines counts complete lines in path; a missing file counts zero.
func CountLines(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return bytes.Count(data, []byte("\n")), nil
}
