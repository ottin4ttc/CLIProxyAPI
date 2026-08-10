package convstore

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func testStore(t *testing.T) (*Store, string, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	clock := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	cfg, _ := ParseConfig(nil)
	cfg.DataDir = dir
	s := New(cfg, func() time.Time { return clock })
	t.Cleanup(s.Shutdown)
	return s, dir, &clock
}

func readLines(t *testing.T, dir string) []Line {
	t.Helper()
	var lines []Line
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return err
		}
		data, _ := os.ReadFile(path)
		for _, raw := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if raw == "" {
				continue
			}
			var l Line
			if err := json.Unmarshal([]byte(raw), &l); err != nil {
				t.Fatalf("bad line %q: %v", raw, err)
			}
			lines = append(lines, l)
		}
		return nil
	})
	return lines
}

func readSingleLine(t *testing.T, st *Store) map[string]any {
	t.Helper()
	var rawLine string
	_ = filepath.Walk(st.cfg.DataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return err
		}
		data, _ := os.ReadFile(path)
		for _, raw := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if raw != "" {
				rawLine = raw
				break
			}
		}
		return nil
	})
	if rawLine == "" {
		t.Fatalf("no JSONL line found")
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(rawLine), &line); err != nil {
		t.Fatalf("bad line %q: %v", rawLine, err)
	}
	return line
}

func reqIntercept(body string) pluginapi.RequestInterceptRequest {
	return pluginapi.RequestInterceptRequest{
		RequestID:    "req-1",
		SourceFormat: "claude",
		Model:        "claude-sonnet-5",
		Headers:      http.Header{"Authorization": {"Bearer zhangsan-sk-abcdefgh12345"}},
		Body:         []byte(body),
	}
}

func TestNonStreamingRoundTrip(t *testing.T) {
	s, dir, _ := testStore(t)
	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	s.OnRequestBefore(reqIntercept(body))
	s.OnResponse(pluginapi.ResponseInterceptRequest{
		RequestID:       "req-1",
		OriginalRequest: []byte(body),
		Body:            []byte(`{"id":"msg_1","content":[{"type":"text","text":"hello"}]}`),
		StatusCode:      200,
	})
	s.Shutdown() // flush writer
	lines := readLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	l := lines[0]
	if l.Status != "ok" || l.Stream || l.Response == "" {
		t.Fatalf("unexpected line: %+v", l)
	}
	// Directory label must keep the user prefix readable (16+4 truncation).
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "zhangsan-sk-abcd_2345" {
		t.Fatalf("unexpected key dir: %v", entries)
	}
}

// TestDuplicateRequestIDFinalizesStaleAsError is a regression test for the
// RequestID-keyed pendings map: a second OnRequestBefore for the same id
// (host retry) must not silently overwrite the first pending. Instead the
// stale entry is finalized as "error" so the retry is visible in the log,
// and the second request/response pair still records normally.
func TestDuplicateRequestIDFinalizesStaleAsError(t *testing.T) {
	s, dir, clock := testStore(t)
	body := `{"messages":[{"role":"user","content":"same"}]}`
	s.OnRequestBefore(reqIntercept(body))
	// Same RequestID means the stale and retry records would otherwise share
	// a filename ({ts}-{requestID}.jsonl); advance the clock so each lands
	// in its own file instead of the retry silently overwriting the stale one.
	*clock = clock.Add(time.Millisecond)
	s.OnRequestBefore(reqIntercept(body)) // same RequestID: stale pending finalized as "error"
	s.OnResponse(pluginapi.ResponseInterceptRequest{RequestID: "req-1", OriginalRequest: []byte(body), Body: []byte(`{"n":2}`)})
	s.Shutdown()
	lines := readLines(t, dir)
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	var gotError, gotOK bool
	for _, l := range lines {
		switch l.Status {
		case "error":
			gotError = true
		case "ok":
			gotOK = true
		}
	}
	if !gotError || !gotOK {
		t.Fatalf("want one error line (stale) and one ok line (completed), got %+v", lines)
	}
}

func TestUnmatchedResponseDropped(t *testing.T) {
	s, dir, _ := testStore(t)
	s.OnResponse(pluginapi.ResponseInterceptRequest{RequestID: "req-none", OriginalRequest: []byte(`{"x":1}`), Body: []byte(`{}`)})
	s.Shutdown()
	if lines := readLines(t, dir); len(lines) != 0 {
		t.Fatalf("unmatched response must not produce lines: %+v", lines)
	}
}

func TestBodyTruncation(t *testing.T) {
	s, dir, _ := testStore(t)
	s.Reconfigure(func() Config { c, _ := ParseConfig(nil); c.DataDir = s.cfg.DataDir; c.MaxBodyBytes = 10; return c }())
	body := `{"messages":[{"role":"user","content":"aaaaaaaaaaaaaaaaaaaaaaaa"}]}`
	s.OnRequestBefore(reqIntercept(body))
	s.OnResponse(pluginapi.ResponseInterceptRequest{RequestID: "req-1", OriginalRequest: []byte(body), Body: []byte(`{"long":"bbbbbbbbbbbbbbbbbbbbbb"}`)})
	s.Shutdown()
	lines := readLines(t, dir)
	if len(lines) != 1 || !lines[0].TruncatedBody {
		t.Fatalf("expected truncated_body line: %+v", lines)
	}
	if len(lines[0].Response) > 10 {
		t.Fatalf("response not truncated: %q", lines[0].Response)
	}
}

// TestConcurrentFinalizeSameSessionFile is a regression test verifying that
// N concurrent finalizes into the same session directory each land in their
// own file, uncorrupted, with no record lost or overwritten by another.
func TestConcurrentFinalizeSameSessionFile(t *testing.T) {
	s, dir, _ := testStore(t)
	const n = 20

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			// Same session header (so all turns land in one session file) but
			// a distinct RequestID, keeping each goroutine's
			// OnRequestBefore/OnResponse pair from crossing with another's.
			body := fmt.Sprintf(`{"model":"claude-sonnet-5","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"},{"role":"assistant","content":"marker-%d"}]}`, i)
			req := reqIntercept(body)
			req.RequestID = fmt.Sprintf("req-%d", i)
			req.Headers.Set("X-Claude-Code-Session-Id", "concurrent-session")
			s.OnRequestBefore(req)
			s.OnResponse(pluginapi.ResponseInterceptRequest{
				RequestID:       req.RequestID,
				OriginalRequest: []byte(body),
				Body:            []byte(fmt.Sprintf(`{"n":%d}`, i)),
				StatusCode:      200,
			})
		}(i)
	}
	wg.Wait()
	s.Shutdown()

	lines := readLines(t, dir)
	if len(lines) != n {
		t.Fatalf("want %d lines, got %d", n, len(lines))
	}

	var files []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".jsonl") {
			files = append(files, path)
		}
		return err
	})
	if len(files) != n {
		t.Fatalf("want %d distinct request files, got %d: %v", n, len(files), files)
	}
	for _, f := range files {
		if got := filepath.Base(filepath.Dir(f)); got != "concurrent-session" {
			t.Fatalf("file %s sits under %q, want session dir concurrent-session", f, got)
		}
	}
}

func TestOneFilePerRequest(t *testing.T) {
	s, dir, _ := testStore(t)
	for i, id := range []string{"req-1", "req-2"} {
		headers := http.Header{}
		headers.Set("session_id", "sess-abc")
		s.OnRequestBefore(pluginapi.RequestInterceptRequest{
			RequestID: id,
			Headers:   headers,
			Body:      []byte(fmt.Sprintf(`{"n":%d}`, i)),
		})
		s.OnResponse(pluginapi.ResponseInterceptRequest{
			RequestID: id,
			Body:      []byte(`{"ok":true}`),
		})
	}
	s.writer.Close()

	var files []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d: %v", len(files), files)
	}
	for _, f := range files {
		if got := filepath.Base(filepath.Dir(f)); got != "sess-abc" {
			t.Fatalf("file %s sits under %q, want session dir sess-abc", f, got)
		}
		data, errRead := os.ReadFile(f)
		if errRead != nil {
			t.Fatalf("read %s: %v", f, errRead)
		}
		if n := strings.Count(strings.TrimSpace(string(data)), "\n"); n != 0 {
			t.Fatalf("file %s holds %d extra newlines, want exactly one record", f, n)
		}
	}
}

func TestLineHasNoTurnField(t *testing.T) {
	s, _, _ := testStore(t)
	headers := http.Header{}
	headers.Set("session_id", "sess-turn")
	s.OnRequestBefore(pluginapi.RequestInterceptRequest{
		RequestID: "req-1",
		Headers:   headers,
		Body:      []byte(`{}`),
	})
	s.OnResponse(pluginapi.ResponseInterceptRequest{
		RequestID: "req-1",
		Body:      []byte(`{}`),
	})
	s.writer.Close()

	line := readSingleLine(t, s)
	if _, ok := line["turn"]; ok {
		t.Fatal("turn field is still present in the record")
	}
	if _, ok := line["ts"]; !ok {
		t.Fatal("ts field is required for ordering")
	}
}

func TestConcurrentRequestsInOneSessionWriteDistinctFiles(t *testing.T) {
	s, dir, _ := testStore(t)
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("req-%d", i)
			headers := http.Header{}
			headers.Set("session_id", "sess-concurrent")
			s.OnRequestBefore(pluginapi.RequestInterceptRequest{
				RequestID: id,
				Headers:   headers,
				Body:      []byte(`{}`),
			})
			s.OnResponse(pluginapi.ResponseInterceptRequest{
				RequestID: id,
				Body:      []byte(`{}`),
			})
		}(i)
	}
	wg.Wait()
	s.writer.Close()

	sessionDirs, err := filepath.Glob(filepath.Join(dir, "*", "sess-concurrent"))
	if err != nil || len(sessionDirs) != 1 {
		t.Fatalf("want exactly 1 session dir, got %v (err %v)", sessionDirs, err)
	}
	entries, err := os.ReadDir(sessionDirs[0])
	if err != nil {
		t.Fatalf("read session dir: %v", err)
	}
	if len(entries) != n {
		t.Fatalf("want %d distinct files, got %d", n, len(entries))
	}
}

func TestRequestHeadersMaskedAndRecorded(t *testing.T) {
	st := New(func() Config { c, _ := ParseConfig(nil); c.DataDir = t.TempDir(); return c }(), func() time.Time { return time.Unix(1700000000, 0) })
	t.Cleanup(st.Shutdown)
	h := http.Header{
		"Authorization": []string{"Bearer sk-verysecretkey1234"},
		"User-Agent":    []string{"codex-cli/0.147.0"},
	}
	st.OnRequestBefore(pluginapi.RequestInterceptRequest{RequestID: "r1", Headers: h, Body: []byte(`{}`)})
	st.OnResponse(pluginapi.ResponseInterceptRequest{RequestID: "r1", Body: []byte(`{"ok":true}`)})
	st.Shutdown()
	line := readSingleLine(t, st)
	headers := line["request_headers"].(map[string]any)
	if headers["User-Agent"] != "codex-cli/0.147.0" {
		t.Fatalf("plain header altered: %v", headers["User-Agent"])
	}
	auth := headers["Authorization"].(string)
	if auth == "Bearer sk-verysecretkey1234" || !strings.HasPrefix(auth, "Bearer ") {
		t.Fatalf("authorization not masked correctly: %q", auth)
	}
}
