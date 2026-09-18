package app

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// withTempHome redirects appDir so codexJSONLPath writes into a per-test
// temp dir. Returns the resolved JSONL path for assertions.
func withTempHome(t *testing.T, threadID string) string {
	t.Helper()
	tmp := t.TempDir()
	origAppDir := appDir
	appDir = tmp
	t.Cleanup(func() { appDir = origAppDir })
	return filepath.Join(tmp, "codex", threadID+".jsonl")
}

func readAllLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open jsonl: %v", err)
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024), 4*1024*1024)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan jsonl: %v", err)
	}
	return lines
}

func TestCodexJSONLPath_EmptyThreadID(t *testing.T) {
	if got := codexJSONLPath(""); got != "" {
		t.Fatalf("expected empty path for empty threadID, got %q", got)
	}
}

func TestWriteCodexUserMessage_RoundTrip(t *testing.T) {
	tid := "test-thread-user-001"
	path := withTempHome(t, tid)

	writeCodexUserMessage(tid, "你是谁, 心脏用的是什么")
	writeCodexUserMessage(tid, "second turn")

	lines := readAllLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}

	for i, want := range []string{"你是谁, 心脏用的是什么", "second turn"} {
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &ev); err != nil {
			t.Fatalf("line %d: unmarshal: %v", i, err)
		}
		if ev.Type != "user" {
			t.Errorf("line %d: type=%q, want user", i, ev.Type)
		}
		if ev.Message.Role != "user" {
			t.Errorf("line %d: role=%q, want user", i, ev.Message.Role)
		}
		if ev.Message.Content != want {
			t.Errorf("line %d: content=%q, want %q", i, ev.Message.Content, want)
		}
	}
}

func TestWriteCodexAssistantMessage_TextBlock(t *testing.T) {
	tid := "test-thread-assistant-001"
	path := withTempHome(t, tid)

	writeCodexAssistantMessage(tid, "gpt-5.5", "我是未然，心脏用的是 codex/gpt-5.5。")

	lines := readAllLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}

	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Model   string `json:"model"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Type != "assistant" {
		t.Errorf("type=%q, want assistant", ev.Type)
	}
	if ev.Message.Model != "gpt-5.5" {
		t.Errorf("model=%q, want gpt-5.5", ev.Message.Model)
	}
	if len(ev.Message.Content) != 1 {
		t.Fatalf("content blocks=%d, want 1", len(ev.Message.Content))
	}
	if ev.Message.Content[0].Type != "text" {
		t.Errorf("block type=%q, want text", ev.Message.Content[0].Type)
	}
	if !strings.Contains(ev.Message.Content[0].Text, "未然") {
		t.Errorf("text missing expected substring: %q", ev.Message.Content[0].Text)
	}
}

func TestWriteCodexSystemInit_Schema(t *testing.T) {
	tid := "test-thread-init-001"
	path := withTempHome(t, tid)

	writeCodexSystemInit(tid, "gpt-5.5", "/tmp/cwd")

	lines := readAllLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"type", "subtype", "sessionId", "model", "cwd", "timestamp", "backend"} {
		if _, ok := ev[k]; !ok {
			t.Errorf("missing field %q in init line: %v", k, ev)
		}
	}
	if ev["type"] != "system" || ev["subtype"] != "init" {
		t.Errorf("unexpected system kind: type=%v subtype=%v", ev["type"], ev["subtype"])
	}
	if ev["sessionId"] != tid {
		t.Errorf("sessionId=%v, want %s", ev["sessionId"], tid)
	}
}

func TestWriteCodexToolResult_Pairing(t *testing.T) {
	tid := "test-thread-tool-001"
	path := withTempHome(t, tid)

	writeCodexAssistantBlock(tid, "gpt-5.5", map[string]any{
		"type":  "tool_use",
		"id":    "tool-42",
		"name":  "Bash",
		"input": map[string]any{"command": "echo hi"},
	})
	writeCodexToolResult(tid, "tool-42", "hi\n", false)

	lines := readAllLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	// tool_use should be assistant, tool_result should be user (matches CC schema)
	var first, second map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first unmarshal: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("second unmarshal: %v", err)
	}
	if first["type"] != "assistant" {
		t.Errorf("tool_use line type=%v, want assistant", first["type"])
	}
	if second["type"] != "user" {
		t.Errorf("tool_result line type=%v, want user", second["type"])
	}
}

func TestAppendCodexJSONL_ConcurrentWriters(t *testing.T) {
	tid := "test-thread-concurrent-001"
	path := withTempHome(t, tid)

	const writers = 8
	const perWriter = 25

	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				writeCodexUserMessage(tid, "msg")
			}
		}(w)
	}
	wg.Wait()

	lines := readAllLines(t, path)
	if want := writers * perWriter; len(lines) != want {
		t.Fatalf("expected %d lines, got %d (corruption / interleaving?)", want, len(lines))
	}
	// Every line must round-trip as JSON — the per-thread lock prevents
	// interleaving that would create truncated objects.
	for i, line := range lines {
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Errorf("line %d invalid JSON: %v\nbody: %s", i, err, line)
		}
	}
}

func TestExtractCodexResultText_Shapes(t *testing.T) {
	cases := []struct {
		name string
		in   json.RawMessage
		want string
	}{
		{"empty", nil, ""},
		{"text_wrapper", json.RawMessage(`{"text":"hello"}`), "hello"},
		{"object_falls_through", json.RawMessage(`{"foo":1}`), `{"foo":1}`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := extractCodexResultText(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
