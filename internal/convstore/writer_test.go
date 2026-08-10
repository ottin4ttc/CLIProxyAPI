package convstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLineMarshalFieldNames(t *testing.T) {
	l := Line{TS: 5, Stream: true, SessionSource: "fingerprint",
		Status: "ok", Request: json.RawMessage(`{"a":1}`), Response: "data: x\n"}
	raw, err := l.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ts", "stream", "session_source", "status", "request", "response"} {
		if _, ok := m[want]; !ok {
			t.Errorf("missing field %q in %s", want, raw)
		}
	}
	if _, ok := m["model"]; ok {
		t.Error("empty model should be omitted")
	}
}

func TestWriterWriteCreatesDirAndFile(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "label", "sess")
	w := NewWriter(16)
	w.Write(sessionDir, "req-1.jsonl", []byte(`{"ts":1}`))
	w.Write(sessionDir, "req-2.jsonl", []byte(`{"ts":2}`))
	w.Close()
	data1, err := os.ReadFile(filepath.Join(sessionDir, "req-1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data1) != "{\"ts\":1}\n" {
		t.Fatalf("unexpected content: %q", data1)
	}
	data2, err := os.ReadFile(filepath.Join(sessionDir, "req-2.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data2) != "{\"ts\":2}\n" {
		t.Fatalf("unexpected content: %q", data2)
	}
}

// TestWriterConcurrentWriteDuringClose races several goroutines calling
// Write against a single Close. Must not panic ("send on closed channel")
// under -race.
func TestWriterConcurrentWriteDuringClose(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "sess")
	w := NewWriter(16)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				w.Write(sessionDir, fmt.Sprintf("req-%d-%d.jsonl", n, j), []byte(`{"ts":1}`))
			}
		}(i)
	}

	// Close concurrently with the Write storm above.
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

// TestWriterWriteAfterCloseDrops verifies that Write after Close is a
// silent no-op (dropped and logged) rather than a panic, and no file is
// created for the dropped record.
func TestWriterWriteAfterCloseDrops(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "sess")
	w := NewWriter(4)
	w.Write(sessionDir, "req-1.jsonl", []byte(`{"ts":1}`))
	w.Close()

	w.Write(sessionDir, "req-2.jsonl", []byte(`{"ts":2}`))

	if _, err := os.Stat(filepath.Join(sessionDir, "req-2.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("write after Close should be dropped, got err=%v", err)
	}
}

// TestWriterHasPendingTracksInFlightWrites verifies HasPending reports true
// once a write is enqueued and false again once the writer goroutine has
// drained it. The writer goroutine is started manually (rather than via
// NewWriter) so the "enqueued but not yet written" window can be observed
// deterministically instead of racing the background goroutine.
func TestWriterHasPendingTracksInFlightWrites(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "sess")
	w := &Writer{queue: make(chan writeRequest, 4), done: make(chan struct{}), pending: make(map[string]int)}

	w.Write(sessionDir, "req-1.jsonl", []byte(`{"ts":1}`))
	if !w.HasPending(sessionDir) {
		t.Fatal("expected HasPending true immediately after enqueue, before the writer goroutine drains it")
	}

	go w.run()
	w.Close() // waits for the queue to drain before returning.

	if w.HasPending(sessionDir) {
		t.Fatal("expected HasPending false after Close drains the queue")
	}
}
