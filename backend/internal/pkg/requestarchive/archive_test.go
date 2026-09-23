package requestarchive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var archiveTime = time.Date(2026, 9, 21, 16, 1, 2, 123000000, time.UTC)

func newTestScope(t *testing.T, root string) *Scope {
	t.Helper()
	if err := Initialize(Config{Root: root}, archiveTime); err != nil {
		t.Fatal(err)
	}
	_, s := WithScope(context.Background(), Config{Root: root, ProviderCode: "custom"}, "session-1", archiveTime)
	t.Cleanup(s.Finish)
	return s
}
func readMeta(t *testing.T, dir string) Metadata {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m Metadata
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(dir, name))
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func consume(t *testing.T, r io.ReadCloser, want string) {
	t.Helper()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != want {
		t.Fatal("upstream bytes changed")
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestSyncPreservesRawJSONAndMetadata(t *testing.T) {
	root := t.TempDir()
	s := newTestScope(t, root)
	if entries, _ := os.ReadDir(filepath.Join(root, "2026-09-22")); len(entries) != 0 {
		t.Fatal("request archive created before response")
	}
	request := `{ "model":"real-model", "extra":9007199254740993 }`
	raw := "{ \"choices\": [ {\"text\":\"" + strings.Repeat("x", 70000) + "\"} ], \"usage\":{\"prompt_tokens\":120,\"completion_tokens\":85},\"extra\":9007199254740993 }"
	r := s.Wrap(io.NopCloser(strings.NewReader(raw)), []byte(request), Info{Protocol: "OPENAI", Model: "real-model", Platform: "deepseek", Status: 200}, false, int64(len(raw)))
	consume(t, r, raw)
	s.Finish()
	dir := filepath.Join(root, "2026-09-22", "req_20260922_000001")
	if readFile(t, dir, "biz_request.json") != request || readFile(t, dir, "biz_response.json") != raw {
		t.Fatal("archived JSON differs from the upstream payload")
	}
	m := readMeta(t, dir)
	if m.RequestID != "req_20260922_000001" || m.SessionID == nil || *m.SessionID != "session-1" || m.RequestTime != "2026-09-22 00:01:02" || m.Protocol != "OPENAI" || *m.ModelName != "real-model" || *m.Platform != "deepseek" || *m.ProviderCode != "custom" || m.HTTPStatus != 200 || m.RequestMode != "SYNC" || *m.InputTokens != 120 || *m.OutputTokens != 85 {
		t.Fatalf("metadata: %+v", m)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readFile(t, dir, "metadata.json")), &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["archiveStatus"]; ok {
		t.Fatal("archiveStatus must not be saved")
	}
	if _, ok := fields["responseComplete"]; ok {
		t.Fatal("responseComplete must not be saved")
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 3 {
		t.Fatalf("files: %v", files)
	}
}

type tinyReader struct{ r io.Reader }

func (r tinyReader) Read(p []byte) (int, error) {
	if len(p) > 3 {
		p = p[:3]
	}
	return r.r.Read(p)
}
func TestSSEPreservesEachPayloadDoneAndLargeEvents(t *testing.T) {
	root := t.TempDir()
	s := newTestScope(t, root)
	first := `{ "choices":[{"delta":{"content":"` + strings.Repeat("字", 30000) + `"}}],"precise":9007199254740993 }`
	last := `{"usage":{"prompt_tokens":120,"completion_tokens":85}}`
	raw := ": ping\r\nevent: completion\r\nid: ignored\r\ndata: " + first + "\r\n\r\ndata:" + last + "\n\ndata: [DONE]\n\n"
	r := s.Wrap(io.NopCloser(tinyReader{strings.NewReader(raw)}), []byte(`{"stream":true}`), Info{Protocol: "OPENAI", Stream: true, Status: 200}, true, -1)
	consume(t, r, raw)
	s.Finish()
	dir := filepath.Join(root, "2026-09-22", "req_20260922_000001")
	if got := readFile(t, dir, "biz_stream_response.jsonl"); got != first+"\n"+last+"\n[DONE]\n" {
		t.Fatal("SSE archive differs from upstream events or DONE was lost")
	}
	m := readMeta(t, dir)
	if *m.InputTokens != 120 || *m.OutputTokens != 85 {
		t.Fatalf("metadata: %+v", m)
	}
}

func TestSSEDispatchesFinalEventAtEOFWithoutBlankLine(t *testing.T) {
	root := t.TempDir()
	s := newTestScope(t, root)
	raw := "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}"
	r := s.Wrap(io.NopCloser(strings.NewReader(raw)), []byte(`{"stream":true}`), Info{Protocol: "OPENAI", Stream: true, Status: 200}, true, -1)
	consume(t, r, raw)
	s.Finish()
	dir := filepath.Join(root, "2026-09-22", "req_20260922_000001")
	if got := readFile(t, dir, "biz_stream_response.jsonl"); got != "{\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n" {
		t.Fatalf("unexpected archive: %q", got)
	}
	m := readMeta(t, dir)
	if m.InputTokens == nil || *m.InputTokens != 2 || m.OutputTokens == nil || *m.OutputTokens != 1 {
		t.Fatalf("metadata: %+v", m)
	}
}
func TestRetryReplacesOnlyItsOwnAttempt(t *testing.T) {
	root := t.TempDir()
	s := newTestScope(t, root)
	first := s.Wrap(io.NopCloser(strings.NewReader(`{"error":"retry"}`)), []byte(`{"model":"old"}`), Info{Status: 429}, false, -1)
	consume(t, first, `{"error":"retry"}`)
	stream := "data: {\"model\":\"new\"}\n\ndata: [DONE]\n\n"
	second := s.Wrap(io.NopCloser(strings.NewReader(stream)), []byte(`{"model":"new","stream":true}`), Info{Protocol: "OPENAI", Model: "new", Platform: "qwen", Stream: true, Status: 200}, true, -1)
	consume(t, second, stream)
	s.Finish()
	dir := filepath.Join(root, "2026-09-22", "req_20260922_000001")
	if _, e := os.Stat(filepath.Join(dir, "biz_response.json")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("old attempt response remains")
	}
	if readFile(t, dir, "biz_request.json") != `{"model":"new","stream":true}` {
		t.Fatal("wrong attempt request")
	}
	m := readMeta(t, dir)
	if *m.ModelName != "new" || m.HTTPStatus != 200 {
		t.Fatalf("metadata: %+v", m)
	}
}
func TestConcurrentCollisionNeverOverwrites(t *testing.T) {
	root := t.TempDir()
	if err := Initialize(Config{Root: root}, archiveTime); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, s := WithScope(context.Background(), Config{Root: root}, "", archiveTime)
			defer s.Finish()
			v := fmt.Sprintf(`{"value":%d}`, i)
			r := s.Wrap(io.NopCloser(strings.NewReader(v)), []byte(v), Info{Status: 200}, false, -1)
			_, _ = io.Copy(io.Discard, r)
			_ = r.Close()
		}(i)
	}
	wg.Wait()
	entries, err := os.ReadDir(filepath.Join(root, "2026-09-22"))
	if err != nil || len(entries) != 20 {
		t.Fatalf("archives=%d err=%v", len(entries), err)
	}
	for _, entry := range entries {
		dir := filepath.Join(root, "2026-09-22", entry.Name())
		if readFile(t, dir, "biz_request.json") != readFile(t, dir, "biz_response.json") {
			t.Fatal("concurrent requests mixed")
		}
		files, _ := os.ReadDir(dir)
		if len(files) != 3 {
			t.Fatalf("files: %v", files)
		}
	}
}
func TestUniqueConcurrentRequests(t *testing.T) {
	root := t.TempDir()
	if err := Initialize(Config{Root: root}, archiveTime); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, s := WithScope(context.Background(), Config{Root: root}, "", archiveTime)
			defer s.Finish()
			r := s.Wrap(io.NopCloser(strings.NewReader(`{}`)), []byte(`{}`), Info{Status: 200}, false, -1)
			_, _ = io.Copy(io.Discard, r)
			_ = r.Close()
		}(i)
	}
	wg.Wait()
	entries, err := os.ReadDir(filepath.Join(root, "2026-09-22"))
	if err != nil || len(entries) != 30 {
		t.Fatalf("archives=%d err=%v", len(entries), err)
	}
}

func TestInitializeRestoresSequenceAndDayRolloverResetsIt(t *testing.T) {
	root := t.TempDir()
	dayOneFinal := filepath.Join(root, "2026-09-22")
	dayOneStaging := filepath.Join(root, ".staging", "2026-09-22")
	if err := os.MkdirAll(filepath.Join(dayOneFinal, "req_20260922_000007"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dayOneStaging, "req_20260922_000009"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(Config{Root: root}, archiveTime); err != nil {
		t.Fatal(err)
	}

	_, first := WithScope(context.Background(), Config{Root: root}, "", archiveTime)
	response := first.Wrap(io.NopCloser(strings.NewReader(`{}`)), []byte(`{}`), Info{Status: 200}, false, 2)
	consume(t, response, `{}`)
	first.Finish()
	if _, err := os.Stat(filepath.Join(dayOneFinal, "req_20260922_000010")); err != nil {
		t.Fatalf("sequence was not restored from disk: %v", err)
	}

	dayTwo := archiveTime.Add(24 * time.Hour)
	_, second := WithScope(context.Background(), Config{Root: root}, "", dayTwo)
	response = second.Wrap(io.NopCloser(strings.NewReader(`{}`)), []byte(`{}`), Info{Status: 200}, false, 2)
	consume(t, response, `{}`)
	second.Finish()
	if _, err := os.Stat(filepath.Join(root, "2026-09-23", "req_20260923_000001")); err != nil {
		t.Fatalf("new day did not reset sequence: %v", err)
	}
}

func TestSequenceConflictSkipsArchiveWithoutRetry(t *testing.T) {
	root := t.TempDir()
	if err := Initialize(Config{Root: root}, archiveTime); err != nil {
		t.Fatal(err)
	}
	conflict := filepath.Join(root, ".staging", "2026-09-22", "req_20260922_000001")
	if err := os.Mkdir(conflict, 0700); err != nil {
		t.Fatal(err)
	}
	before := FailureCount(FailureReasonSequenceConflict)
	_, scope := WithScope(context.Background(), Config{Root: root}, "", archiveTime)
	original := io.NopCloser(strings.NewReader(`{"ok":true}`))
	wrapped := scope.Wrap(original, []byte(`{}`), Info{Status: 200}, false, -1)
	consume(t, wrapped, `{"ok":true}`)
	scope.Finish()

	if got := FailureCount(FailureReasonSequenceConflict); got != before+1 {
		t.Fatalf("sequence conflict metric=%d, want %d", got, before+1)
	}
	if _, err := os.Stat(filepath.Join(root, "2026-09-22", "req_20260922_000002")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("conflicting allocation retried with the next sequence")
	}
}

func TestFinishKeepsSharedStagingParents(t *testing.T) {
	root := t.TempDir()
	scope := newTestScope(t, root)
	response := scope.Wrap(io.NopCloser(strings.NewReader(`{}`)), []byte(`{}`), Info{Status: 200}, false, 2)
	consume(t, response, `{}`)
	scope.Finish()
	if info, err := os.Stat(filepath.Join(root, ".staging", "2026-09-22")); err != nil || !info.IsDir() {
		t.Fatalf("shared staging parent was removed: %v", err)
	}
}

func TestIncompleteStreamAndFailOpen(t *testing.T) {
	t.Run("partial", func(t *testing.T) {
		root := t.TempDir()
		s := newTestScope(t, root)
		raw := "data: {\"ok\":true}\n\ndata: {\"unfinished\":"
		r := s.Wrap(io.NopCloser(strings.NewReader(raw)), []byte(`{}`), Info{Stream: true, Status: 200}, true, -1)
		consume(t, r, raw)
		s.Finish()
		entries, err := os.ReadDir(filepath.Join(root, "2026-09-22"))
		if err != nil || len(entries) != 0 {
			t.Fatalf("incomplete archive was published: %v", entries)
		}
	})
	t.Run("invalid archive root fails initialization", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(root, []byte("sentinel"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Initialize(Config{Root: root}, archiveTime); err == nil {
			t.Fatal("invalid archive root was accepted")
		}
	})
}
func TestScopeFinishClosesInterruptedCapture(t *testing.T) {
	root := t.TempDir()
	s := newTestScope(t, root)
	raw := "data: {\"delta\":\"part\"}\n\n"
	r := s.Wrap(io.NopCloser(strings.NewReader(raw)), []byte(`{}`), Info{Stream: true, Status: 200}, true, -1)
	b := make([]byte, len(raw))
	if _, err := io.ReadFull(r, b); err != nil {
		t.Fatal(err)
	}
	s.Finish()
	_ = r.Close()
	entries, err := os.ReadDir(filepath.Join(root, "2026-09-22"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("interrupted archive was published: %v", entries)
	}
}
func TestMissingUsageAndSessionRemainNull(t *testing.T) {
	root := t.TempDir()
	if err := Initialize(Config{Root: root}, archiveTime); err != nil {
		t.Fatal(err)
	}
	_, s := WithScope(context.Background(), Config{Root: root}, "", archiveTime)
	defer s.Finish()
	r := s.Wrap(io.NopCloser(strings.NewReader(`{"error":"denied"}`)), []byte(`{}`), Info{Status: 403}, false, -1)
	consume(t, r, `{"error":"denied"}`)
	s.Finish()
	m := readMeta(t, filepath.Join(root, "2026-09-22", "req_20260922_000001"))
	if m.SessionID != nil || m.InputTokens != nil || m.OutputTokens != nil || m.ProviderCode != nil {
		t.Fatalf("metadata: %+v", m)
	}
}

func TestAuthoritativeUsageUpdatesClosedCapture(t *testing.T) {
	root := t.TempDir()
	s := newTestScope(t, root)
	r := s.Wrap(io.NopCloser(strings.NewReader(`{"choices":[]}`)), []byte(`{}`), Info{Protocol: "OPENAI", Status: 200}, false, -1)
	consume(t, r, `{"choices":[]}`)
	s.SetUsage(41, 9)
	s.Finish()
	m := readMeta(t, filepath.Join(root, "2026-09-22", "req_20260922_000001"))
	if m.InputTokens == nil || *m.InputTokens != 41 || m.OutputTokens == nil || *m.OutputTokens != 9 {
		t.Fatalf("metadata: %+v", m)
	}
}
func TestTokenTotalsAcrossProtocols(t *testing.T) {
	tests := []struct {
		name, protocol, raw string
		input, output       int64
	}{
		{"openai", "OPENAI", `{"usage":{"prompt_tokens":120,"completion_tokens":85,"prompt_tokens_details":{"cached_tokens":60},"completion_tokens_details":{"reasoning_tokens":20}}}`, 120, 85},
		{"responses", "OPENAI_RESPONSES", `{"type":"response.completed","response":{"usage":{"input_tokens":20,"output_tokens":9}}}`, 20, 9},
		{"anthropic", "ANTHROPIC", `{"usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"output_tokens":8}}`, 60, 8},
		{"gemini", "VERTEX", `{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":30,"thoughtsTokenCount":5,"cachedContentTokenCount":20}}`, 100, 35},
		{"gemini envelope", "VERTEX", `{"response":{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2}}}`, 5, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &capture{}
			c.observeUsage([]byte(tt.raw))
			in, out := c.usage.totals(tt.protocol)
			if in == nil || out == nil || *in != tt.input || *out != tt.output {
				t.Fatalf("totals: %v %v", in, out)
			}
		})
	}
	c := &capture{}
	c.observeUsage([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":10,"output_tokens":0}}}`))
	c.observeUsage([]byte(`{"type":"message_delta","usage":{"output_tokens":15}}`))
	in, out := c.usage.totals("ANTHROPIC")
	if *in != 15 || *out != 15 {
		t.Fatal("message_delta reset input counts")
	}
}
