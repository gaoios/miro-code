package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExtractToolPath(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		input    string
		expected string
	}{
		{"Read file_path", "Read", `{"file_path":"/tmp/a.md"}`, "/tmp/a.md"},
		{"Edit file_path", "Edit", `{"file_path":"/tmp/b.md","old_string":"x","new_string":"y"}`, "/tmp/b.md"},
		{"Grep path", "Grep", `{"pattern":"TODO","path":"/src"}`, "/src"},
		{"Glob path", "Glob", `{"pattern":"*.go","path":"/x"}`, "/x"},
		{"Bash no path", "Bash", `{"command":"ls"}`, ""},
		{"empty input", "Read", `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractToolPath(tc.tool, json.RawMessage(tc.input))
			if got != tc.expected {
				t.Errorf("got %q want %q", got, tc.expected)
			}
		})
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		// no **
		{"*.go", "/tmp/main.go", true}, // basename match
		{"*.go", "/tmp/main.py", false},
		{"/tmp/*.md", "/tmp/a.md", true},

		// leading **
		{"**/*.jsonl", "/a/b/c/foo.jsonl", true},
		{"**/*.jsonl", "foo.jsonl", true},
		{"**/feedback_*.md", "/home/k/memory/topics/feedback_x.md", true},
		{"**/feedback_*.md", "/home/k/memory/topics/other.md", false},

		// deeply nested
		{"**/.claude/projects/**/*.jsonl", "/Users/k/.claude/projects/abc/s.jsonl", true},
		{"**/.claude/projects/**/*.jsonl", "/Users/k/.claude/projects/abc/def/s.jsonl", true},
		{"**/.claude/projects/**/*.jsonl", "/Users/k/.claude/notes/s.jsonl", false},

		// daily note pattern
		{"**/memory/20*-*.md", "/Users/k/memory/2026-04-17.md", true},
		{"**/memory/20*-*.md", "/Users/k/memory/topics/feedback_x.md", false},
	}
	for _, tc := range cases {
		got := globMatch(tc.pattern, tc.path)
		if got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestLoadToolHookConfig_MissingFile(t *testing.T) {
	tmp := t.TempDir()
	cfg, err := loadToolHookConfig(filepath.Join(tmp, "nope.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if cfg == nil || len(cfg.Rules) != 0 {
		t.Errorf("expected empty config for missing file")
	}
}

func TestLoadToolHookConfig_Defaults(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "rules.yaml")
	os.WriteFile(p, []byte(`rules:
  - id: r1
    match: ["*.md"]
    inject: "hello"
`), 0644)
	cfg, err := loadToolHookConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Budget != 1500 {
		t.Errorf("global budget default: got %d want 1500", cfg.Budget)
	}
	if len(cfg.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(cfg.Rules))
	}
	r := cfg.Rules[0]
	if r.Budget != 500 {
		t.Errorf("rule budget default: got %d", r.Budget)
	}
	if r.Priority != 50 {
		t.Errorf("rule priority default: got %d", r.Priority)
	}
	if r.Dedupe != "per_file" {
		t.Errorf("dedupe default: got %q", r.Dedupe)
	}
}

func TestLoadToolHookConfig_PrioritySort(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "rules.yaml")
	os.WriteFile(p, []byte(`rules:
  - id: low
    match: ["*"]
    inject: a
    priority: 10
  - id: high
    match: ["*"]
    inject: b
    priority: 100
  - id: mid
    match: ["*"]
    inject: c
    priority: 50
`), 0644)
	cfg, err := loadToolHookConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Rules[0].ID != "high" || cfg.Rules[1].ID != "mid" || cfg.Rules[2].ID != "low" {
		t.Errorf("priority sort wrong: %v", cfg.Rules)
	}
}

func TestToolMatches(t *testing.T) {
	cases := []struct {
		rule ToolHookRule
		tool string
		want bool
	}{
		{ToolHookRule{Tools: nil}, "Read", true},
		{ToolHookRule{Tools: []string{"Read"}}, "Read", true},
		{ToolHookRule{Tools: []string{"Read"}}, "Edit", false},
		{ToolHookRule{Tools: []string{"Read", "Edit"}}, "Edit", true},
		{ToolHookRule{Tools: []string{"read"}}, "Read", true}, // case-insensitive
	}
	for _, tc := range cases {
		got := tc.rule.toolMatches(tc.tool)
		if got != tc.want {
			t.Errorf("tool=%s tools=%v: got %v want %v", tc.tool, tc.rule.Tools, got, tc.want)
		}
	}
}

func TestRunToolHook_NoMatch_WritesAuditRow(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	// empty config
	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	// invoke hook with input that matches no rule
	input := ToolHookInput{
		SessionID: "test-sess",
		CWD:       "/tmp",
		ToolName:  "Read",
		ToolInput: json.RawMessage(`{"file_path":"/no/match/file.txt"}`),
	}
	payload, _ := json.Marshal(input)

	r, w, _ := os.Pipe()
	w.Write(payload)
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	// capture stdout (expect empty — no injection)
	origStdout := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	runToolHook()
	pw.Close()
	os.Stdout = origStdout

	buf := make([]byte, 1024)
	n, _ := pr.Read(buf)
	out := string(buf[:n])
	if strings.Contains(out, "additionalContext") {
		t.Errorf("expected no injection, got %q", out)
	}

	// verify audit row was written
	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var cnt int
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit WHERE session_id='test-sess'`).Scan(&cnt)
	if cnt != 1 {
		t.Errorf("expected 1 audit row, got %d", cnt)
	}
}

func TestRunToolHook_Match_InjectsAndAudits(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	// write rule matching /tmp/test.md
	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()
	rulePath := filepath.Join(dir, "tool-hooks.yaml")
	os.WriteFile(rulePath, []byte(`rules:
  - id: test_rule
    tools: [Read]
    match: ["**/*.md"]
    inject: "hello from rule"
    dedupe: per_file
`), 0644)

	input := ToolHookInput{
		SessionID: "s1",
		ToolName:  "Read",
		ToolInput: json.RawMessage(`{"file_path":"/tmp/test.md"}`),
	}
	payload, _ := json.Marshal(input)

	// first call — should inject
	r, w, _ := os.Pipe()
	w.Write(payload)
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	pr, pw, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = pw
	runToolHook()
	pw.Close()
	os.Stdin = origStdin
	os.Stdout = origStdout
	buf := make([]byte, 2048)
	n, _ := pr.Read(buf)
	out := string(buf[:n])
	if !strings.Contains(out, "hello from rule") {
		t.Errorf("first call: expected injection, got %q", out)
	}
	if !strings.Contains(out, "hookSpecificOutput") || !strings.Contains(out, "additionalContext") {
		t.Errorf("first call: missing hookSpecificOutput/additionalContext wrapper: %q", out)
	}

	// second call — dedupe per_file should skip injection
	r2, w2, _ := os.Pipe()
	w2.Write(payload)
	w2.Close()
	os.Stdin = r2
	pr2, pw2, _ := os.Pipe()
	os.Stdout = pw2
	runToolHook()
	pw2.Close()
	os.Stdin = origStdin
	os.Stdout = origStdout
	buf2 := make([]byte, 2048)
	n2, _ := pr2.Read(buf2)
	out2 := string(buf2[:n2])
	if strings.Contains(out2, "hello from rule") {
		t.Errorf("second call: expected dedupe skip, got %q", out2)
	}

	// verify audit: 1 injected + 1 dedupe skip
	db, _ := openDB()
	defer db.Close()
	var injected, skipped int
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit WHERE rule_id='test_rule' AND injected=1`).Scan(&injected)
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit WHERE rule_id='test_rule' AND skip_reason='dedupe'`).Scan(&skipped)
	if injected != 1 {
		t.Errorf("injected rows: got %d want 1", injected)
	}
	if skipped != 1 {
		t.Errorf("dedupe rows: got %d want 1", skipped)
	}
}

func TestRunToolHook_MatchInput_Bash(t *testing.T) {
	// match_input lets Bash (which has no path) fire on command content regex.
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()
	rulePath := filepath.Join(dir, "tool-hooks.yaml")
	os.WriteFile(rulePath, []byte(`rules:
  - id: kubectl_reminder
    tools: [Bash]
    match_input: ['\bkubectl\b', '\bk\s+get\b']
    inject: "kubectl reminder text"
    dedupe: never
`), 0644)

	run := func(cmd string) string {
		input := ToolHookInput{
			SessionID: "s1",
			ToolName:  "Bash",
			ToolInput: json.RawMessage(`{"command":` + quoteJSON(cmd) + `}`),
		}
		payload, _ := json.Marshal(input)
		r, w, _ := os.Pipe()
		w.Write(payload)
		w.Close()
		origStdin := os.Stdin
		os.Stdin = r
		pr, pw, _ := os.Pipe()
		origStdout := os.Stdout
		os.Stdout = pw
		runToolHook()
		pw.Close()
		os.Stdin = origStdin
		os.Stdout = origStdout
		buf := make([]byte, 2048)
		n, _ := pr.Read(buf)
		return string(buf[:n])
	}

	// Positive: kubectl command should inject
	out := run("kubectl get pods")
	if !strings.Contains(out, "kubectl reminder text") {
		t.Errorf("kubectl call: expected injection, got %q", out)
	}

	// Positive: `k get pods` (alias) should also inject via second regex
	out = run("k get pods -A")
	if !strings.Contains(out, "kubectl reminder text") {
		t.Errorf("k alias call: expected injection, got %q", out)
	}

	// Negative: unrelated command should NOT inject
	out = run("ls -la /tmp")
	if strings.Contains(out, "kubectl reminder text") {
		t.Errorf("ls call: expected no injection, got %q", out)
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ── PostToolUse: tool_response regex / input regex / path glob filters ──

func TestRunToolHook_PostToolUse_MatchResponse(t *testing.T) {
	// PostToolUse rule fires on tool_response regex.
	// Models the production rule `bash_error_reflex` (#1).
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()
	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	rulePath := filepath.Join(dir, "tool-hooks.yaml")
	os.WriteFile(rulePath, []byte(`rules:
  - id: bash_error_reflex
    events: [PostToolUse]
    tools: [Bash]
    match_response: ['"is_error"\s*:\s*true']
    inject: "Bash failed — check error before continuing"
    dedupe: never
`), 0644)

	run := func(toolName, input, response string) string {
		in := ToolHookInput{
			SessionID:     "s1",
			HookEventName: "PostToolUse",
			ToolName:      toolName,
			ToolInput:     json.RawMessage(input),
			ToolResponse:  json.RawMessage(response),
		}
		payload, _ := json.Marshal(in)
		r, w, _ := os.Pipe()
		w.Write(payload)
		w.Close()
		origStdin := os.Stdin
		os.Stdin = r
		pr, pw, _ := os.Pipe()
		origStdout := os.Stdout
		os.Stdout = pw
		runToolHook()
		pw.Close()
		os.Stdin = origStdin
		os.Stdout = origStdout
		buf := make([]byte, 2048)
		n, _ := pr.Read(buf)
		return string(buf[:n])
	}

	// Positive: is_error:true should inject
	out := run("Bash", `{"command":"false"}`, `{"is_error":true,"stderr":"command failed"}`)
	if !strings.Contains(out, "Bash failed") {
		t.Errorf("error response: expected injection, got %q", out)
	}

	// Negative: success response should NOT inject
	out = run("Bash", `{"command":"true"}`, `{"is_error":false,"stdout":"ok"}`)
	if strings.Contains(out, "Bash failed") {
		t.Errorf("success response: expected no injection, got %q", out)
	}

	// Negative: different tool should NOT inject (Edit has no Bash filter)
	out = run("Edit", `{"file_path":"/tmp/x"}`, `{"is_error":true}`)
	if strings.Contains(out, "Bash failed") {
		t.Errorf("Edit tool: expected no injection, got %q", out)
	}
}

func TestRunToolHook_PostToolUse_MatchInput_Bash(t *testing.T) {
	// PostToolUse rule fires on tool_input regex (Bash command pattern).
	// Models the production rule `notify_dedupe_warn` (#2).
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()
	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	rulePath := filepath.Join(dir, "tool-hooks.yaml")
	os.WriteFile(rulePath, []byte(`rules:
  - id: notify_dedupe_warn
    events: [PostToolUse]
    tools: [Bash]
    match_input: ['weiran\s+notify']
    inject: "notify already sent this session"
    dedupe: per_session
`), 0644)

	run := func(cmd string) string {
		in := ToolHookInput{
			SessionID:     "s1",
			HookEventName: "PostToolUse",
			ToolName:      "Bash",
			ToolInput:     json.RawMessage(`{"command":` + quoteJSON(cmd) + `}`),
			ToolResponse:  json.RawMessage(`{"stdout":"ok"}`),
		}
		payload, _ := json.Marshal(in)
		r, w, _ := os.Pipe()
		w.Write(payload)
		w.Close()
		origStdin := os.Stdin
		os.Stdin = r
		pr, pw, _ := os.Pipe()
		origStdout := os.Stdout
		os.Stdout = pw
		runToolHook()
		pw.Close()
		os.Stdin = origStdin
		os.Stdout = origStdout
		buf := make([]byte, 2048)
		n, _ := pr.Read(buf)
		return string(buf[:n])
	}

	// Positive: weiran notify command should inject (first time)
	out := run(`weiran notify "task done"`)
	if !strings.Contains(out, "notify already sent") {
		t.Errorf("first notify: expected injection, got %q", out)
	}

	// Negative: second call in same session should NOT inject (per_session dedupe)
	out = run(`weiran notify "another update"`)
	if strings.Contains(out, "notify already sent") {
		t.Errorf("second notify same session: expected dedupe skip, got %q", out)
	}

	// Negative: unrelated command should NOT inject
	out = run("ls -la /tmp")
	if strings.Contains(out, "notify already sent") {
		t.Errorf("unrelated command: expected no injection, got %q", out)
	}
}

func TestRunToolHook_PostToolUse_SkipInput_Suppresses(t *testing.T) {
	// PostToolUse rules with skip_input must suppress the rule when tool_input
	// matches a skip pattern, even if match_input already matched.
	// Regression: skip_input was originally only checked in PreToolUse handler;
	// PostToolUse rules' skip_input was silently ignored, causing self-referential
	// triggers on `echo '... <kw> ...'` and `weiran tool-hook` smoke tests.
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()
	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	rulePath := filepath.Join(dir, "tool-hooks.yaml")
	os.WriteFile(rulePath, []byte(`rules:
  - id: make_install_warn
    events: [PostToolUse]
    tools: [Bash]
    match_input: ['"command":\s*"[^"]*\bmake\s+install\b']
    skip_input:
      - '"command":\s*"\s*(echo|printf|cat)\b'
      - '"command":\s*"[^"]*\bweiran\s+tool-hook\b'
    inject: "make install warning"
    dedupe: never
`), 0644)

	run := func(sessionID, cmd string) string {
		in := ToolHookInput{
			SessionID:     sessionID,
			HookEventName: "PostToolUse",
			ToolName:      "Bash",
			ToolInput:     json.RawMessage(`{"command":` + quoteJSON(cmd) + `}`),
			ToolResponse:  json.RawMessage(`{"stdout":"ok"}`),
		}
		payload, _ := json.Marshal(in)
		r, w, _ := os.Pipe()
		w.Write(payload)
		w.Close()
		origStdin := os.Stdin
		os.Stdin = r
		pr, pw, _ := os.Pipe()
		origStdout := os.Stdout
		os.Stdout = pw
		runToolHook()
		pw.Close()
		os.Stdin = origStdin
		os.Stdout = origStdout
		buf := make([]byte, 2048)
		n, _ := pr.Read(buf)
		return string(buf[:n])
	}

	// Positive: real `make install` should inject
	out := run("s_real", "cd ~/scripts/weiran && make install")
	if !strings.Contains(out, "make install warning") {
		t.Errorf("real make install: expected injection, got %q", out)
	}

	// Negative: echo wrapping the same string should be suppressed by skip_input
	out = run("s_echo", `echo "demo of make install command"`)
	if strings.Contains(out, "make install warning") {
		t.Errorf("echo wrapper: expected skip_input suppression, got %q", out)
	}

	// Negative: weiran tool-hook self-test should be suppressed
	out = run("s_hook", `weiran tool-hook test Bash "make install"`)
	if strings.Contains(out, "make install warning") {
		t.Errorf("weiran tool-hook: expected skip_input suppression, got %q", out)
	}

	// Negative: printf wrapper should also skip
	out = run("s_printf", `printf '%s' "make install reference"`)
	if strings.Contains(out, "make install warning") {
		t.Errorf("printf wrapper: expected skip_input suppression, got %q", out)
	}
}

func TestRunToolHook_PostToolUse_NoFilter_Skips(t *testing.T) {
	// PostToolUse rule with NO match/match_input/match_response should be inert.
	// Prevents accidentally global rules from firing on every tool call.
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()
	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	rulePath := filepath.Join(dir, "tool-hooks.yaml")
	os.WriteFile(rulePath, []byte(`rules:
  - id: no_filter_rule
    events: [PostToolUse]
    tools: [Bash]
    inject: "should not fire"
    dedupe: never
`), 0644)

	in := ToolHookInput{
		SessionID:     "s1",
		HookEventName: "PostToolUse",
		ToolName:      "Bash",
		ToolInput:     json.RawMessage(`{"command":"echo hi"}`),
		ToolResponse:  json.RawMessage(`{"stdout":"hi"}`),
	}
	payload, _ := json.Marshal(in)
	r, w, _ := os.Pipe()
	w.Write(payload)
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	pr, pw, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = pw
	runToolHook()
	pw.Close()
	os.Stdin = origStdin
	os.Stdout = origStdout
	buf := make([]byte, 2048)
	n, _ := pr.Read(buf)
	out := string(buf[:n])

	if strings.Contains(out, "should not fire") {
		t.Errorf("rule with no filter fired: got %q", out)
	}
}

// ── Action: mark_restart_initiator ──

func TestExtractBashCommand(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"plain command", `{"command":"make server-restart"}`, "make server-restart"},
		{"with description", `{"command":"ls -la","description":"list files"}`, "ls -la"},
		{"trims whitespace", `{"command":"  make server-restart  "}`, "make server-restart"},
		{"empty input", `{}`, ""},
		{"malformed JSON", `not json`, ""},
		{"empty bytes", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractBashCommand(json.RawMessage(tc.input))
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestActionMarkRestartInitiator_Success(t *testing.T) {
	// Reset server DB singleton + point appDir at a fresh temp dir.
	serverDB = nil
	serverDBOnce = sync.Once{}
	tmpDir := t.TempDir()
	origAppDir := appDir
	appDir = tmpDir
	t.Cleanup(func() { appDir = origAppDir })

	db, err := openServerDB()
	if err != nil {
		t.Fatalf("openServerDB: %v", err)
	}

	// Insert a session row with a known claude_session_id.
	now := time.Now().Format(time.RFC3339)
	_, err = db.Exec(`INSERT INTO server_sessions
		(session_id, name, claude_session_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		"weiran-sid-abc", "test", "cc-sid-xyz", now, now)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Fire the action.
	in := ToolHookInput{
		SessionID: "cc-sid-xyz",
		ToolName:  "Bash",
		ToolInput: json.RawMessage(`{"command":"make server-restart"}`),
	}
	actionMarkRestartInitiator("test_rule", in)

	// Verify rehydrate_message column was populated for the right session.
	var msg string
	err = db.QueryRow(`SELECT rehydrate_message FROM server_sessions WHERE session_id=?`,
		"weiran-sid-abc").Scan(&msg)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(msg, "make server-restart") {
		t.Errorf("expected msg to mention command, got %q", msg)
	}
	if !strings.Contains(msg, "you triggered") {
		t.Errorf("expected attribution wording, got %q", msg)
	}
	if !strings.Contains(msg, "do NOT re-run") {
		t.Errorf("expected anti-rerun warning, got %q", msg)
	}
}

func TestActionMarkRestartInitiator_NoSession(t *testing.T) {
	// Reset DB; leave server_sessions empty so lookup must return "" gracefully.
	serverDB = nil
	serverDBOnce = sync.Once{}
	tmpDir := t.TempDir()
	origAppDir := appDir
	appDir = tmpDir
	t.Cleanup(func() { appDir = origAppDir })

	if _, err := openServerDB(); err != nil {
		t.Fatalf("openServerDB: %v", err)
	}

	// Should not panic; should silently skip when CC sid has no weiran mapping.
	in := ToolHookInput{
		SessionID: "cc-sid-unknown",
		ToolName:  "Bash",
		ToolInput: json.RawMessage(`{"command":"make server-restart"}`),
	}
	actionMarkRestartInitiator("test_rule", in)
}

func TestActionMarkRestartInitiator_EmptySID(t *testing.T) {
	// Empty CC sid (e.g. hook payload outside any session) should early-return.
	in := ToolHookInput{
		SessionID: "",
		ToolName:  "Bash",
		ToolInput: json.RawMessage(`{"command":"make server-restart"}`),
	}
	actionMarkRestartInitiator("test_rule", in) // must not panic
}

func TestActionMarkRestartInitiator_LongCommandTruncated(t *testing.T) {
	serverDB = nil
	serverDBOnce = sync.Once{}
	tmpDir := t.TempDir()
	origAppDir := appDir
	appDir = tmpDir
	t.Cleanup(func() { appDir = origAppDir })

	db, _ := openServerDB()
	now := time.Now().Format(time.RFC3339)
	_, _ = db.Exec(`INSERT INTO server_sessions
		(session_id, name, claude_session_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		"weiran-sid-long", "test", "cc-sid-long", now, now)

	// 500-char command — should get truncated to 200 + "…" in the message.
	longCmd := strings.Repeat("x", 500)
	in := ToolHookInput{
		SessionID: "cc-sid-long",
		ToolName:  "Bash",
		ToolInput: json.RawMessage(`{"command":` + quoteJSON(longCmd) + `}`),
	}
	actionMarkRestartInitiator("test_rule", in)

	var msg string
	db.QueryRow(`SELECT rehydrate_message FROM server_sessions WHERE session_id=?`,
		"weiran-sid-long").Scan(&msg)
	if !strings.Contains(msg, "…") {
		t.Errorf("expected truncation marker, got %q", msg)
	}
	if strings.Count(msg, "x") > 220 {
		t.Errorf("command should be truncated to ~200 chars, got %d x's", strings.Count(msg, "x"))
	}
}

func TestRunRuleAction_UnknownAction(t *testing.T) {
	// Unknown actions log to stderr but must not panic or affect anything.
	in := ToolHookInput{
		SessionID: "any",
		ToolName:  "Bash",
		ToolInput: json.RawMessage(`{"command":"echo"}`),
	}
	runRuleAction("nonexistent_action_xyz", "test_rule", in) // must not panic
}

// TestRunToolHook_Action_MarkRestartInitiator wires the action through the full
// hook pipeline (YAML rule → match → audit → action dispatch). Verifies that
// a Bash call matching `make server-restart` results in:
//  1. Standard inject output (system-reminder body)
//  2. Side effect: rehydrate_message persisted to server_sessions
func TestRunToolHook_Action_MarkRestartInitiator(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	// Reset & point server DB at the same dir.
	serverDB = nil
	serverDBOnce = sync.Once{}
	origAppDir := appDir
	appDir = dir
	defer func() { appDir = origAppDir }()

	// Seed a session row that the action will look up.
	sdb, err := openServerDB()
	if err != nil {
		t.Fatalf("openServerDB: %v", err)
	}
	now := time.Now().Format(time.RFC3339)
	_, err = sdb.Exec(`INSERT INTO server_sessions
		(session_id, name, claude_session_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		"weiran-int-1", "intg", "cc-int-1", now, now)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Write hook config with an action rule.
	rulePath := filepath.Join(dir, "tool-hooks.yaml")
	os.WriteFile(rulePath, []byte(`rules:
  - id: server_restart_attribution
    tools: [Bash]
    match_input: ['make\s+server-restart']
    action: mark_restart_initiator
    inject: "marked as initiator"
    dedupe: never
`), 0644)

	// Invoke the hook.
	input := ToolHookInput{
		SessionID:     "cc-int-1",
		ToolName:      "Bash",
		ToolInput:     json.RawMessage(`{"command":"make server-restart"}`),
		HookEventName: "PreToolUse",
	}
	payload, _ := json.Marshal(input)

	r, w, _ := os.Pipe()
	w.Write(payload)
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	pr, pw, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = pw
	runToolHook()
	pw.Close()
	os.Stdin = origStdin
	os.Stdout = origStdout

	buf := make([]byte, 4096)
	n, _ := pr.Read(buf)
	out := string(buf[:n])

	// 1. Standard inject path — should still emit the system-reminder body.
	if !strings.Contains(out, "marked as initiator") {
		t.Errorf("expected inject body in stdout, got %q", out)
	}

	// 2. Side effect — rehydrate_message persisted.
	var msg string
	err = sdb.QueryRow(`SELECT rehydrate_message FROM server_sessions WHERE session_id=?`,
		"weiran-int-1").Scan(&msg)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(msg, "make server-restart") {
		t.Errorf("expected rehydrate_message to mention command, got %q", msg)
	}
	if !strings.Contains(msg, "you triggered") {
		t.Errorf("expected attribution wording, got %q", msg)
	}
}

// TestRunToolHook_Action_NoMatch_NoSideEffect ensures the action only fires
// when the rule's match conditions pass — a non-matching command must not
// touch rehydrate_message.
func TestRunToolHook_Action_NoMatch_NoSideEffect(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	serverDB = nil
	serverDBOnce = sync.Once{}
	origAppDir := appDir
	appDir = dir
	defer func() { appDir = origAppDir }()

	sdb, _ := openServerDB()
	now := time.Now().Format(time.RFC3339)
	sdb.Exec(`INSERT INTO server_sessions
		(session_id, name, claude_session_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		"weiran-nm", "test", "cc-nm", now, now)

	rulePath := filepath.Join(dir, "tool-hooks.yaml")
	os.WriteFile(rulePath, []byte(`rules:
  - id: server_restart_attribution
    tools: [Bash]
    match_input: ['make\s+server-restart']
    action: mark_restart_initiator
    inject: "would mark"
    dedupe: never
`), 0644)

	// Run an unrelated command.
	input := ToolHookInput{
		SessionID:     "cc-nm",
		ToolName:      "Bash",
		ToolInput:     json.RawMessage(`{"command":"ls -la"}`),
		HookEventName: "PreToolUse",
	}
	payload, _ := json.Marshal(input)
	r, w, _ := os.Pipe()
	w.Write(payload)
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	pr, pw, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = pw
	runToolHook()
	pw.Close()
	os.Stdin = origStdin
	os.Stdout = origStdout
	discard := make([]byte, 1024)
	pr.Read(discard)

	// rehydrate_message must remain empty.
	var msg string
	sdb.QueryRow(`SELECT rehydrate_message FROM server_sessions WHERE session_id=?`,
		"weiran-nm").Scan(&msg)
	if msg != "" {
		t.Errorf("expected empty rehydrate_message, got %q", msg)
	}
}

func TestQueryToolHookStats(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "stats.db")
	defer func() { dbPath = origDB }()

	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Seed rows inside the query window. Keep these relative so the test does
	// not expire as wall-clock time moves on.
	base := time.Now().Add(-24 * time.Hour)
	rows := []toolHookAuditRow{
		{Timestamp: base.Format(time.RFC3339), SessionID: "s1", ToolName: "Read", Path: "/a.md", RuleID: "r1", Injected: true, InjectionSize: 100, LatencyMS: 5},
		{Timestamp: base.Add(time.Minute).Format(time.RFC3339), SessionID: "s1", ToolName: "Read", Path: "/a.md", RuleID: "r1", Injected: false, SkipReason: "dedupe", LatencyMS: 3},
		{Timestamp: base.Add(2 * time.Minute).Format(time.RFC3339), SessionID: "s1", ToolName: "Edit", Path: "/b.md", LatencyMS: 2},
	}
	for _, r := range rows {
		writeToolHookAudit(db, r)
	}

	stats, err := queryToolHookStats(db, 7)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalCalls != 3 {
		t.Errorf("total: got %d want 3", stats.TotalCalls)
	}
	if stats.InjectedCalls != 1 {
		t.Errorf("injected: got %d want 1", stats.InjectedCalls)
	}
	if stats.MatchedCalls != 2 {
		t.Errorf("matched: got %d want 2", stats.MatchedCalls)
	}
	if stats.ByTool["Read"] != 2 || stats.ByTool["Edit"] != 1 {
		t.Errorf("by_tool: got %v", stats.ByTool)
	}
	if stats.SkipBreakdown["dedupe"] != 1 {
		t.Errorf("skip: got %v", stats.SkipBreakdown)
	}
	if r1 := stats.ByRule["r1"]; r1.Calls != 2 || r1.Injected != 1 {
		t.Errorf("r1: got %+v", r1)
	}
}

// ── UserPromptSubmit dispatch tests ──

func TestEventMatches(t *testing.T) {
	cases := []struct {
		name   string
		rule   ToolHookRule
		event  string
		expect bool
	}{
		{"empty events defaults to PreToolUse", ToolHookRule{}, HookEventPreToolUse, true},
		{"empty events does not match UserPromptSubmit", ToolHookRule{}, HookEventUserPromptSubmit, false},
		{"explicit match", ToolHookRule{Events: []string{"UserPromptSubmit"}}, HookEventUserPromptSubmit, true},
		{"explicit no-match", ToolHookRule{Events: []string{"UserPromptSubmit"}}, HookEventPreToolUse, false},
		{"case-insensitive", ToolHookRule{Events: []string{"userpromptsubmit"}}, HookEventUserPromptSubmit, true},
		{"multi-event list", ToolHookRule{Events: []string{"PreToolUse", "UserPromptSubmit"}}, HookEventUserPromptSubmit, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.rule.eventMatches(tc.event)
			if got != tc.expect {
				t.Errorf("got %v want %v", got, tc.expect)
			}
		})
	}
}

func TestMatchesPromptRegex(t *testing.T) {
	cases := []struct {
		name     string
		prompt   string
		patterns []string
		want     bool
	}{
		{"empty prompt", "", []string{"foo"}, false},
		{"empty patterns", "hello", nil, false},
		{"simple match", "请记住这件事", []string{"记住|记下"}, true},
		{"case-insensitive flag", "REMEMBER this", []string{"(?i)remember this"}, true},
		{"case-sensitive mismatch", "REMEMBER this", []string{"remember this"}, false},
		{"no match", "天气真好", []string{"记住|记下"}, false},
		{"bad regex skipped (no crash)", "记住", []string{"[invalid", "记住"}, true},
		{"bad regex no match", "天气", []string{"[invalid"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := matchesPromptRegex(tc.prompt, tc.patterns)
			if got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestRunUserPromptSubmitHook_Match(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "ups.db")
	defer func() { dbPath = origDB }()

	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	os.WriteFile(filepath.Join(dir, "tool-hooks.yaml"), []byte(`rules:
  - id: remember_signal
    events: [UserPromptSubmit]
    match_prompt:
      - '记住|记下|remember this'
    inject: "write to file, do not just reply"
    dedupe: never
`), 0644)

	input := ToolHookInput{
		SessionID:     "ups-sess",
		HookEventName: "UserPromptSubmit",
		Prompt:        "主人说 请记住 以后都这样",
	}
	payload, _ := json.Marshal(input)

	r, w, _ := os.Pipe()
	w.Write(payload)
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	pr, pw, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = pw
	runToolHook()
	pw.Close()
	os.Stdout = origStdout

	buf := make([]byte, 2048)
	n, _ := pr.Read(buf)
	out := string(buf[:n])

	if !strings.Contains(out, "write to file") {
		t.Errorf("expected injection, got %q", out)
	}
	if !strings.Contains(out, `"hookEventName":"UserPromptSubmit"`) {
		t.Errorf("expected UserPromptSubmit event name in output, got %q", out)
	}

	db, _ := openDB()
	defer db.Close()
	var n1 int
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit WHERE rule_id='remember_signal' AND injected=1 AND event_name='UserPromptSubmit'`).Scan(&n1)
	if n1 != 1 {
		t.Errorf("expected 1 UserPromptSubmit audit row, got %d", n1)
	}
}

func TestRunUserPromptSubmitHook_NoMatch(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "ups-nomatch.db")
	defer func() { dbPath = origDB }()

	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	os.WriteFile(filepath.Join(dir, "tool-hooks.yaml"), []byte(`rules:
  - id: remember_signal
    events: [UserPromptSubmit]
    match_prompt: ['记住|记下']
    inject: "write to file"
`), 0644)

	input := ToolHookInput{
		SessionID:     "ups-nomatch",
		HookEventName: "UserPromptSubmit",
		Prompt:        "今天天气怎么样",
	}
	payload, _ := json.Marshal(input)

	r, w, _ := os.Pipe()
	w.Write(payload)
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	pr, pw, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = pw
	runToolHook()
	pw.Close()
	os.Stdout = origStdout

	buf := make([]byte, 1024)
	n, _ := pr.Read(buf)
	out := string(buf[:n])
	if strings.Contains(out, "additionalContext") {
		t.Errorf("expected no injection, got %q", out)
	}

	// Observability row still written (no rule matched)
	db, _ := openDB()
	defer db.Close()
	var cnt int
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit WHERE session_id='ups-nomatch' AND event_name='UserPromptSubmit'`).Scan(&cnt)
	if cnt != 1 {
		t.Errorf("expected 1 observability row, got %d", cnt)
	}
}

func TestRunStopHook_Match(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "stop.db")
	defer func() { dbPath = origDB }()

	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	os.WriteFile(filepath.Join(dir, "tool-hooks.yaml"), []byte(`rules:
  - id: end_of_turn_reminder
    events: [Stop]
    inject: "leave a hook in your reply, ask about next step"
    dedupe: never
`), 0644)

	input := ToolHookInput{
		SessionID:     "stop-sess",
		HookEventName: "Stop",
	}
	payload, _ := json.Marshal(input)

	r, w, _ := os.Pipe()
	w.Write(payload)
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	pr, pw, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = pw
	runToolHook()
	pw.Close()
	os.Stdout = origStdout

	buf := make([]byte, 2048)
	n, _ := pr.Read(buf)
	out := string(buf[:n])

	if !strings.Contains(out, "leave a hook") {
		t.Errorf("expected injection, got %q", out)
	}
	if !strings.Contains(out, `"hookEventName":"Stop"`) {
		t.Errorf("expected Stop event name in output, got %q", out)
	}

	db, _ := openDB()
	defer db.Close()
	var n1 int
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit WHERE rule_id='end_of_turn_reminder' AND injected=1 AND event_name='Stop'`).Scan(&n1)
	if n1 != 1 {
		t.Errorf("expected 1 Stop audit row, got %d", n1)
	}
}

func TestRunStopHook_PerSessionDedupe(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "stop-dedupe.db")
	defer func() { dbPath = origDB }()

	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	os.WriteFile(filepath.Join(dir, "tool-hooks.yaml"), []byte(`rules:
  - id: once_per_session
    events: [Stop]
    inject: "first stop only"
    dedupe: per_session
`), 0644)

	runOne := func() string {
		input := ToolHookInput{
			SessionID:     "stop-dedupe-sess",
			HookEventName: "Stop",
		}
		payload, _ := json.Marshal(input)
		r, w, _ := os.Pipe()
		w.Write(payload)
		w.Close()
		origStdin := os.Stdin
		os.Stdin = r
		defer func() { os.Stdin = origStdin }()

		pr, pw, _ := os.Pipe()
		origStdout := os.Stdout
		os.Stdout = pw
		runToolHook()
		pw.Close()
		os.Stdout = origStdout
		buf := make([]byte, 2048)
		n, _ := pr.Read(buf)
		return string(buf[:n])
	}

	out1 := runOne()
	if !strings.Contains(out1, "first stop only") {
		t.Errorf("first stop should inject, got %q", out1)
	}
	out2 := runOne()
	if strings.Contains(out2, "first stop only") {
		t.Errorf("second stop should be deduped, got %q", out2)
	}

	db, _ := openDB()
	defer db.Close()
	var injCount, dedupeCount int
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit WHERE rule_id='once_per_session' AND injected=1`).Scan(&injCount)
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit WHERE rule_id='once_per_session' AND skip_reason='dedupe'`).Scan(&dedupeCount)
	if injCount != 1 {
		t.Errorf("expected 1 injection, got %d", injCount)
	}
	if dedupeCount != 1 {
		t.Errorf("expected 1 dedupe skip, got %d", dedupeCount)
	}
}

func TestRunStopHook_SubagentStop(t *testing.T) {
	// SubagentStop shares dispatcher with Stop — verify event name is preserved.
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "subagent.db")
	defer func() { dbPath = origDB }()

	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	os.WriteFile(filepath.Join(dir, "tool-hooks.yaml"), []byte(`rules:
  - id: subagent_only
    events: [SubagentStop]
    inject: "subagent done"
    dedupe: never
  - id: stop_only
    events: [Stop]
    inject: "main stop"
    dedupe: never
`), 0644)

	input := ToolHookInput{
		SessionID:     "sub-sess",
		HookEventName: "SubagentStop",
	}
	payload, _ := json.Marshal(input)
	r, w, _ := os.Pipe()
	w.Write(payload)
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	pr, pw, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = pw
	runToolHook()
	pw.Close()
	os.Stdout = origStdout
	buf := make([]byte, 2048)
	n, _ := pr.Read(buf)
	out := string(buf[:n])

	if !strings.Contains(out, "subagent done") {
		t.Errorf("expected SubagentStop rule to fire, got %q", out)
	}
	if strings.Contains(out, "main stop") {
		t.Errorf("Stop-only rule should not fire on SubagentStop, got %q", out)
	}
	if !strings.Contains(out, `"hookEventName":"SubagentStop"`) {
		t.Errorf("expected SubagentStop in output, got %q", out)
	}
}

func TestEventDispatch_BackwardCompat_NoEventName(t *testing.T) {
	// Payloads without hook_event_name must still work as PreToolUse.
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "bc.db")
	defer func() { dbPath = origDB }()

	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	os.WriteFile(filepath.Join(dir, "tool-hooks.yaml"), []byte(`rules:
  - id: legacy_rule
    tools: [Read]
    match: ["**/*.md"]
    inject: "legacy fires"
`), 0644)

	input := ToolHookInput{
		SessionID: "bc-sess",
		ToolName:  "Read",
		ToolInput: json.RawMessage(`{"file_path":"/tmp/x.md"}`),
		// HookEventName intentionally empty
	}
	payload, _ := json.Marshal(input)

	r, w, _ := os.Pipe()
	w.Write(payload)
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	pr, pw, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = pw
	runToolHook()
	pw.Close()
	os.Stdout = origStdout

	buf := make([]byte, 2048)
	n, _ := pr.Read(buf)
	out := string(buf[:n])
	if !strings.Contains(out, "legacy fires") {
		t.Errorf("expected legacy rule injection, got %q", out)
	}
}

func TestPromptDigest(t *testing.T) {
	cases := []struct {
		in  string
		n   int
		out string
	}{
		{"", 10, ""},
		{"short", 10, "short"},
		{"line1\nline2", 100, "line1 line2"},
		{"abcdefghijklmnop", 5, "abcde…"},
		{"中文测试内容", 3, "中文测…"},
	}
	for _, tc := range cases {
		got := promptDigest(tc.in, tc.n)
		if got != tc.out {
			t.Errorf("promptDigest(%q, %d) = %q want %q", tc.in, tc.n, got, tc.out)
		}
	}
}

// TestRunToolHook_Decision_Deny verifies that a rule with `decision: deny`
// emits a permissionDecision JSON (not additionalContext) and short-circuits
// subsequent rules in the same invocation.
func TestRunToolHook_Decision_Deny(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()
	rulePath := filepath.Join(dir, "tool-hooks.yaml")
	// Two rules: high-priority deny + lower-priority context. Deny must win
	// and the context rule must NOT appear in stdout.
	os.WriteFile(rulePath, []byte(`rules:
  - id: bash_deny_redirect
    tools: [Bash]
    match_input: ['\b(cat|echo)\b[^|;&]*>']
    decision: deny
    inject: "use Edit/Write, not Bash redirect"
    dedupe: never
    priority: 100
  - id: bash_context_note
    tools: [Bash]
    match_input: ['\b(cat|echo)\b']
    inject: "context note should not appear"
    dedupe: never
    priority: 10
`), 0644)

	run := func(cmd string) string {
		input := ToolHookInput{
			SessionID: "s-deny-1",
			ToolName:  "Bash",
			ToolInput: json.RawMessage(`{"command":` + quoteJSON(cmd) + `}`),
		}
		payload, _ := json.Marshal(input)
		r, w, _ := os.Pipe()
		w.Write(payload)
		w.Close()
		origStdin := os.Stdin
		os.Stdin = r
		pr, pw, _ := os.Pipe()
		origStdout := os.Stdout
		os.Stdout = pw
		runToolHook()
		pw.Close()
		os.Stdin = origStdin
		os.Stdout = origStdout
		buf := make([]byte, 4096)
		n, _ := pr.Read(buf)
		return string(buf[:n])
	}

	// Positive: `echo foo > bar` matches deny regex
	out := run("echo hello > /tmp/out")
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("expected permissionDecision=deny, got %q", out)
	}
	if !strings.Contains(out, "use Edit/Write") {
		t.Errorf("expected deny reason in output, got %q", out)
	}
	// Short-circuit: the lower-priority context rule must NOT appear
	if strings.Contains(out, "context note should not appear") {
		t.Errorf("deny did not short-circuit, context rule leaked: %q", out)
	}
	// Output must not mix both formats
	if strings.Contains(out, `"additionalContext"`) {
		t.Errorf("decision output should not contain additionalContext: %q", out)
	}

	// Negative: plain `ls` matches neither rule → no output
	out = run("ls -la")
	if strings.TrimSpace(out) != "" {
		t.Errorf("unmatched command should produce no output, got %q", out)
	}

	// Negative: `cat file.txt` (no redirect) matches only the low-prio context
	// rule → should emit additionalContext, NOT permissionDecision
	out = run("cat file.txt")
	if strings.Contains(out, `"permissionDecision"`) {
		t.Errorf("non-redirect cat should not trigger deny, got %q", out)
	}
	if !strings.Contains(out, `"additionalContext"`) {
		t.Errorf("non-redirect cat should produce context, got %q", out)
	}
}

// TestValidDecision sanity-checks the decision whitelist.
func TestValidDecision(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"", true},
		{"deny", true},
		{"ask", true},
		{"allow", true},
		{"DENY", false}, // case-sensitive on purpose (mirrors Claude Code)
		{"block", false},
		{"nope", false},
	}
	for _, tc := range cases {
		if got := validDecision(tc.in); got != tc.ok {
			t.Errorf("validDecision(%q) = %v, want %v", tc.in, got, tc.ok)
		}
	}
}
