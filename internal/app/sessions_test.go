package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionMatchesQuery_SingleWord(t *testing.T) {
	s := sessionInfo{
		Title:   "Fix nginx config",
		Project: "workspace/projects/nginx",
	}
	if !sessionMatchesQuery(s, "nginx") {
		t.Error("should match 'nginx' in title")
	}
	if sessionMatchesQuery(s, "gallery") {
		t.Error("should not match 'gallery'")
	}
}

func TestSessionMatchesQuery_MultiWord(t *testing.T) {
	s := sessionInfo{
		Title:   "Fix nginx config",
		Project: "workspace/projects/nginx",
	}
	if !sessionMatchesQuery(s, "nginx fix") {
		t.Error("should match both words")
	}
	if sessionMatchesQuery(s, "nginx gallery") {
		t.Error("should not match when one word is missing")
	}
}

func TestSessionMatchesQuery_CaseInsensitive(t *testing.T) {
	s := sessionInfo{
		Title: "Docker Compose Setup",
	}
	if !sessionMatchesQuery(s, "docker compose") {
		t.Error("should match case-insensitively")
	}
}

func TestSessionMatchesQuery_MatchesSummary(t *testing.T) {
	s := sessionInfo{
		Summary: "Fixed memory leak in gallery service",
	}
	if !sessionMatchesQuery(s, "memory leak") {
		t.Error("should match words in summary")
	}
}

func TestSessionMatchesQuery_MatchesID(t *testing.T) {
	s := sessionInfo{
		ID: "abc123def456",
	}
	if !sessionMatchesQuery(s, "abc123") {
		t.Error("should match session ID")
	}
}

func TestSessionMatchesQuery_MatchesModel(t *testing.T) {
	s := sessionInfo{
		Model: "claude-3-opus",
	}
	if !sessionMatchesQuery(s, "opus") {
		t.Error("should match model")
	}
}

func TestSessionMatchesQuery_EmptyQuery(t *testing.T) {
	s := sessionInfo{Title: "anything"}
	// empty query → no words → all match
	if !sessionMatchesQuery(s, "") {
		t.Error("empty query should match everything")
	}
}

func TestDecodeProjectName_Standard(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{fmt.Sprintf("-Users-%s--openclaw-workspace", filepath.Base(home)), "~/.openclaw/workspace"},
		{fmt.Sprintf("-Users-%s-projects", filepath.Base(home)), "~/projects"},
		{fmt.Sprintf("-Users-%s-work-myapp", filepath.Base(home)), "~/work/myapp"},
	}
	for _, tt := range tests {
		got := decodeProjectName(tt.input)
		if got != tt.expected {
			t.Errorf("decodeProjectName(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		bytes    int64
		expected string
	}{
		{500, "500B"},
		{1024, "1K"},
		{2048, "2K"},
		{1048576, "1.0M"},
		{1572864, "1.5M"},
		{5242880, "5.0M"},
		{0, "0B"},
	}
	for _, tt := range tests {
		got := humanSize(tt.bytes)
		if got != tt.expected {
			t.Errorf("humanSize(%d) = %q, want %q", tt.bytes, got, tt.expected)
		}
	}
}

func TestExtractText_String(t *testing.T) {
	raw := json.RawMessage(`"hello world"`)
	got := extractText(raw)
	if got != "hello world" {
		t.Errorf("extractText string = %q", got)
	}
}

func TestExtractText_StringTruncation(t *testing.T) {
	long := strings.Repeat("a", 300)
	raw, _ := json.Marshal(long)
	got := extractText(json.RawMessage(raw))
	if len(got) != 200 {
		t.Errorf("extractText should truncate to 200, got %d", len(got))
	}
}

func TestExtractText_ContentBlocks(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"hello from block"},{"type":"image","text":""}]`)
	got := extractText(raw)
	if got != "hello from block" {
		t.Errorf("extractText blocks = %q", got)
	}
}

func TestExtractText_EmptyBlocks(t *testing.T) {
	raw := json.RawMessage(`[{"type":"image","text":""}]`)
	got := extractText(raw)
	if got != "" {
		t.Errorf("extractText empty blocks = %q, want empty", got)
	}
}

func TestExtractText_Invalid(t *testing.T) {
	raw := json.RawMessage(`12345`)
	got := extractText(raw)
	if got != "" {
		t.Errorf("extractText invalid = %q, want empty", got)
	}
}

func TestParseSessionHead(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "session.jsonl")

	lines := []string{
		`{"type":"custom-title","title":"Test Session"}`,
		`{"type":"user","timestamp":"2026-04-05T10:00:00Z","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"hi","model":"claude-opus-4-6"}}`,
		`{"type":"user","message":{"role":"user","content":"second msg"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"reply"}}`,
	}
	var data string
	for _, l := range lines {
		data += l + "\n"
	}
	os.WriteFile(f, []byte(data), 0644)

	s := &sessionInfo{Path: f, Size: int64(len(data))}
	parseSessionHead(s)

	if s.Title != "Test Session" {
		t.Errorf("title = %q, want 'Test Session'", s.Title)
	}
	if s.FirstMsg != "hello" {
		t.Errorf("firstMsg = %q, want 'hello'", s.FirstMsg)
	}
	if s.Model != "claude-opus-4-6" {
		t.Errorf("model = %q, want 'claude-opus-4-6'", s.Model)
	}
	if s.Messages != 4 {
		t.Errorf("messages = %d, want 4", s.Messages)
	}
	if s.StartTime.IsZero() {
		t.Error("startTime should not be zero")
	}
}

func TestParseSessionHead_Empty(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "empty.jsonl")
	os.WriteFile(f, []byte(""), 0644)

	s := &sessionInfo{Path: f}
	parseSessionHead(s)

	if s.Messages != 0 {
		t.Errorf("messages = %d, want 0", s.Messages)
	}
}

func TestParseSessionHead_NonExistent(t *testing.T) {
	s := &sessionInfo{Path: "/nonexistent.jsonl"}
	parseSessionHead(s) // should not panic
	if s.Messages != 0 {
		t.Errorf("messages = %d, want 0", s.Messages)
	}
}

func TestCollect_FiltersByCutoff(t *testing.T) {
	dir := t.TempDir()

	recent := filepath.Join(dir, "recent.jsonl")
	os.WriteFile(recent, []byte(`{"type":"user","message":{"content":"recent msg"}}`), 0644)

	// cutoff in the future — should filter everything
	var results []sessionFile
	collect(&results, dir, "test", time.Now().Add(1*time.Hour))
	if len(results) != 0 {
		t.Errorf("expected 0 results with future cutoff, got %d", len(results))
	}

	// cutoff in the past — should include
	results = nil
	collect(&results, dir, "test", time.Now().Add(-1*time.Hour))
	if len(results) != 0 {
		// The file was just created, so it should be after past cutoff
		// But isOwnSession check might affect this — use non-weiran content
	}
	// Re-test with explicit past cutoff
	results = nil
	collect(&results, dir, "test", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if len(results) != 1 {
		t.Errorf("expected 1 result with far-past cutoff, got %d", len(results))
	}
}

func TestCollect_SkipsWeiranSessions(t *testing.T) {
	dir := t.TempDir()

	wf := filepath.Join(dir, "weiran-sess.jsonl")
	os.WriteFile(wf, []byte(`{"type":"user","message":{"content":"executing boot recall.\n\nscanning these JSONL"}}`), 0644)

	nf := filepath.Join(dir, "normal-sess.jsonl")
	os.WriteFile(nf, []byte(`{"type":"user","message":{"content":"help me with K8s"}}`), 0644)

	var results []sessionFile
	collect(&results, dir, "test", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	if len(results) != 1 {
		t.Errorf("expected 1 result (weiran session skipped), got %d", len(results))
	}
}

func TestCollect_SkipsNonJSONL(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("text"), 0644)
	os.WriteFile(filepath.Join(dir, "test.jsonl"), []byte(`{"type":"user","message":{"content":"hi"}}`), 0644)

	var results []sessionFile
	collect(&results, dir, "test", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	if len(results) != 1 {
		t.Errorf("expected 1 .jsonl file, got %d", len(results))
	}
}

func TestCollect_SkipsDirectories(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "subdir.jsonl"), 0755) // dir with .jsonl name
	os.WriteFile(filepath.Join(dir, "real.jsonl"), []byte(`{"type":"user","message":{"content":"hi"}}`), 0644)

	var results []sessionFile
	collect(&results, dir, "test", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	if len(results) != 1 {
		t.Errorf("expected 1 result, got %d", len(results))
	}
}

func TestFormatSessionList_Empty(t *testing.T) {
	got := formatSessionList(nil)
	if got != "" {
		t.Errorf("expected empty string for nil sessions, got %q", got)
	}
}

func TestFormatSessionList_WithSessions(t *testing.T) {
	sessions := []sessionFile{
		{source: "cc", path: "/tmp/test1.jsonl"},
		{source: "oc-main", path: "/tmp/test2.jsonl"},
	}
	got := formatSessionList(sessions)
	if got == "" {
		t.Error("expected non-empty output")
	}
	if !strings.Contains(got, "cc:/tmp/test1.jsonl") {
		t.Errorf("missing first session in output: %s", got)
	}
	if !strings.Contains(got, "oc-main:/tmp/test2.jsonl") {
		t.Errorf("missing second session in output: %s", got)
	}
}

func TestIsOwnSession_AllMarkers(t *testing.T) {
	dir := t.TempDir()
	for _, marker := range ownSessionMarkers {
		f := filepath.Join(dir, marker+".jsonl")
		os.WriteFile(f, []byte(`{"type":"user","message":{"content":"`+marker+`"}}`), 0644)
		if !isOwnSession(f) {
			t.Errorf("should detect marker %q", marker)
		}
	}
}

func TestPrintSessionTable_Empty(t *testing.T) {
	// Just verify no panic
	printSessionTable(nil, "")
	printSessionTable(nil, "query")
}

func TestPrintSessionTable_WithData(t *testing.T) {
	sessions := []sessionInfo{
		{
			ID:       "abc12345-6789-0000-0000-000000000000",
			Title:    "Test Session",
			Project:  "~/test",
			Size:     1024,
			ModTime:  time.Now(),
			Messages: 5,
		},
	}
	// Just verify no panic
	printSessionTable(sessions, "")
}

func TestPrintSessionTable_LongProject(t *testing.T) {
	sessions := []sessionInfo{
		{
			ID:      "abc12345-6789-0000-0000-000000000000",
			Title:   "x",
			Project: "this/is/a/very/long/project/name/that/should/be/truncated",
			Size:    2048,
			ModTime: time.Now(),
		},
	}
	printSessionTable(sessions, "")
}

func TestPrintSessionTable_FallbackToSummary(t *testing.T) {
	sessions := []sessionInfo{
		{
			ID:      "abc12345-6789-0000-0000-000000000000",
			Summary: "This is a summary",
			Project: "~/test",
			Size:    1024,
			ModTime: time.Now(),
		},
	}
	printSessionTable(sessions, "")
}
