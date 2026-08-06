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

func reqIntercept(body string) pluginapi.RequestInterceptRequest {
	return pluginapi.RequestInterceptRequest{
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
	if l.Status != "ok" || l.Turn != 1 || l.Stream || l.Response == "" {
		t.Fatalf("unexpected line: %+v", l)
	}
	// Directory label must keep the user prefix readable (16+4 truncation).
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "zhangsan-sk-abcd_2345" {
		t.Fatalf("unexpected key dir: %v", entries)
	}
}

func TestDuplicateBodiesMatchFIFO(t *testing.T) {
	s, dir, _ := testStore(t)
	body := `{"messages":[{"role":"user","content":"same"}]}`
	s.OnRequestBefore(reqIntercept(body))
	s.OnRequestBefore(reqIntercept(body))
	s.OnResponse(pluginapi.ResponseInterceptRequest{OriginalRequest: []byte(body), Body: []byte(`{"n":1}`)})
	s.OnResponse(pluginapi.ResponseInterceptRequest{OriginalRequest: []byte(body), Body: []byte(`{"n":2}`)})
	s.Shutdown()
	lines := readLines(t, dir)
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	if lines[0].Turn == lines[1].Turn {
		t.Fatalf("turns must increment: %+v", lines)
	}
}

func TestUnmatchedResponseDropped(t *testing.T) {
	s, dir, _ := testStore(t)
	s.OnResponse(pluginapi.ResponseInterceptRequest{OriginalRequest: []byte(`{"x":1}`), Body: []byte(`{}`)})
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
	s.OnResponse(pluginapi.ResponseInterceptRequest{OriginalRequest: []byte(body), Body: []byte(`{"long":"bbbbbbbbbbbbbbbbbbbbbb"}`)})
	s.Shutdown()
	lines := readLines(t, dir)
	if len(lines) != 1 || !lines[0].TruncatedBody {
		t.Fatalf("expected truncated_body line: %+v", lines)
	}
	if len(lines[0].Response) > 10 {
		t.Fatalf("response not truncated: %q", lines[0].Response)
	}
}

// TestConcurrentFinalizeSameSessionFile is a regression test for the store
// lock no longer being held across finalize's disk I/O (RecoverTornLine /
// CountLines). N goroutines finalize turns for the same brand-new session
// file concurrently; every turn must still be unique and the set of turns
// must be exactly 1..N, even though the seeding I/O for that file races.
func TestConcurrentFinalizeSameSessionFile(t *testing.T) {
	s, dir, _ := testStore(t)
	const n = 20

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			// Same session header (so all turns land in one session file) but
			// a distinct trailing message so each request body hashes
			// differently, keeping each goroutine's OnRequestBefore/OnResponse
			// pair from crossing with another's.
			body := fmt.Sprintf(`{"model":"claude-sonnet-5","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"},{"role":"assistant","content":"marker-%d"}]}`, i)
			req := reqIntercept(body)
			req.Headers.Set("X-Claude-Code-Session-Id", "concurrent-session")
			s.OnRequestBefore(req)
			s.OnResponse(pluginapi.ResponseInterceptRequest{
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
	if len(files) != 1 {
		t.Fatalf("want all turns in 1 session file, got %d: %v", len(files), files)
	}

	seen := make(map[int]bool, n)
	for _, l := range lines {
		if l.Turn < 1 || l.Turn > n {
			t.Fatalf("turn out of range [1,%d]: %+v", n, l)
		}
		if seen[l.Turn] {
			t.Fatalf("duplicate turn %d", l.Turn)
		}
		seen[l.Turn] = true
	}
	if len(seen) != n {
		t.Fatalf("want %d distinct turns, got %d", n, len(seen))
	}
}
