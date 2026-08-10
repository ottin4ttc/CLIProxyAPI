package convstore

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type writeRequest struct {
	dir  string
	name string
	line []byte
}

// Writer serializes all record writes through one goroutine so interceptor
// hooks never block on disk I/O.
type Writer struct {
	mu      sync.Mutex
	closed  bool
	queue   chan writeRequest
	done    chan struct{}
	pending map[string]int // session dir -> enqueued-but-not-yet-written count
}

// NewWriter starts the background write goroutine.
func NewWriter(queueSize int) *Writer {
	w := &Writer{queue: make(chan writeRequest, queueSize), done: make(chan struct{}), pending: make(map[string]int)}
	go w.run()
	return w
}

func (w *Writer) run() {
	defer close(w.done)
	for req := range w.queue {
		path := filepath.Join(req.dir, req.name)
		if err := writeRecord(path, req.line); err != nil {
			fmt.Fprintf(os.Stderr, "[conversation-store] write %s: %v\n", path, err)
		}
		w.decrementPending(req.dir)
	}
}

// decrementPending marks one queued write for dir as done, dropping the
// entry once its count reaches zero.
func (w *Writer) decrementPending(dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n, ok := w.pending[dir]; ok {
		if n <= 1 {
			delete(w.pending, dir)
		} else {
			w.pending[dir] = n - 1
		}
	}
}

// HasPending reports whether dir has a record enqueued but not yet written.
// Maintenance uses this to avoid archiving a directory the writer is about
// to add a file to.
func (w *Writer) HasPending(dir string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pending[dir] > 0
}

// Write enqueues one record as its own file under dir. It never blocks: when
// the queue is full the record is dropped and logged, recording must not
// stall proxying. A copy of line is queued so the caller's backing array is
// never mutated or raced against by the writer goroutine. Writing after
// Close silently drops the record and logs to stderr.
func (w *Writer) Write(dir, name string, line []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		fmt.Fprintf(os.Stderr, "[conversation-store] writer closed, dropping record for %s\n", dir)
		return
	}
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')
	select {
	case w.queue <- writeRequest{dir: dir, name: name, line: buf}:
		w.pending[dir]++
	default:
		fmt.Fprintf(os.Stderr, "[conversation-store] queue full, dropping record for %s\n", dir)
	}
}

// Close drains pending writes and stops the goroutine. It is safe to call
// concurrently with Write and safe to call more than once.
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

// writeRecord appends rather than truncates: a request ID finalized twice
// within the same millisecond (host retry, or a stale pending flushed by a
// repeated ID) produces the same file name, and truncating would silently
// drop the earlier record. Appending keeps both — the file holds two JSON
// lines, which the archiver concatenates correctly either way.
func writeRecord(path string, line []byte) error {
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
