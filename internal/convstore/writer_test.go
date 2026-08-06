package convstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLineMarshalFieldNames(t *testing.T) {
	l := Line{TS: 5, Turn: 1, Stream: true, SessionSource: "fingerprint",
		Status: "ok", Request: json.RawMessage(`{"a":1}`), Response: "data: x\n"}
	raw, err := l.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ts", "turn", "stream", "session_source", "status", "request", "response"} {
		if _, ok := m[want]; !ok {
			t.Errorf("missing field %q in %s", want, raw)
		}
	}
	if _, ok := m["model"]; ok {
		t.Error("empty model should be omitted")
	}
}

func TestWriterAppendCreatesDirsAndAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "label", "sess.jsonl")
	w := NewWriter(16)
	w.Append(path, []byte(`{"ts":1}`))
	w.Append(path, []byte(`{"ts":2}`))
	w.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{\"ts\":1}\n{\"ts\":2}\n" {
		t.Fatalf("unexpected content: %q", data)
	}
}

// TestWriterConcurrentAppendDuringClose races several goroutines calling
// Append against a single Close. Must not panic ("send on closed channel")
// under -race.
func TestWriterConcurrentAppendDuringClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	w := NewWriter(16)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				w.Append(path, []byte(`{"ts":1}`))
			}
		}(i)
	}

	// Close concurrently with the Append storm above.
	go func() {
		w.Close()
	}()

	wg.Wait()
	w.Close() // second Close after the goroutines finish; must not panic.
}

// TestWriterDoubleCloseDoesNotPanic verifies Close is idempotent.
func TestWriterDoubleCloseDoesNotPanic(t *testing.T) {
	w := NewWriter(4)
	w.Close()
	w.Close()
}

// TestWriterAppendAfterCloseDrops verifies that Append after Close is a
// silent no-op (dropped and logged) rather than a panic, and the file is
// left unchanged.
func TestWriterAppendAfterCloseDrops(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	w := NewWriter(4)
	w.Append(path, []byte(`{"ts":1}`))
	w.Close()

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	w.Append(path, []byte(`{"ts":2}`))

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("file changed after Append post-Close: before=%q after=%q", before, after)
	}
}

// TestWriterHasPendingTracksInFlightAppends verifies HasPending reports true
// once an append is enqueued and false again once the writer goroutine has
// drained it. The writer goroutine is started manually (rather than via
// NewWriter) so the "enqueued but not yet written" window can be observed
// deterministically instead of racing the background goroutine.
func TestWriterHasPendingTracksInFlightAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	w := &Writer{queue: make(chan appendRequest, 4), done: make(chan struct{}), pending: make(map[string]int)}

	w.Append(path, []byte(`{"ts":1}`))
	if !w.HasPending(path) {
		t.Fatal("expected HasPending true immediately after enqueue, before the writer goroutine drains it")
	}

	go w.run()
	w.Close() // waits for the queue to drain before returning.

	if w.HasPending(path) {
		t.Fatal("expected HasPending false after Close drains the queue")
	}
}

func TestRecoverTornLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	if err := os.WriteFile(path, []byte("{\"ts\":1}\n{\"ts\":2,\"tor"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RecoverTornLine(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "{\"ts\":1}\n" {
		t.Fatalf("torn line not truncated: %q", data)
	}
	n, err := CountLines(path)
	if err != nil || n != 1 {
		t.Fatalf("CountLines = %d, %v", n, err)
	}
	// Missing file: no error, zero lines.
	if err := RecoverTornLine(filepath.Join(dir, "absent.jsonl")); err != nil {
		t.Fatal(err)
	}
	if n, _ := CountLines(filepath.Join(dir, "absent.jsonl")); n != 0 {
		t.Fatalf("absent file lines = %d", n)
	}
}
