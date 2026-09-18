package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeUntrusted_KeepsNormalText(t *testing.T) {
	input := "Hello World 你好世界\nNew line\ttab"
	got := sanitizeUntrusted(input)
	if got != input {
		t.Errorf("sanitized normal text changed: %q", got)
	}
}

func TestSanitizeUntrusted_StripsControlChars(t *testing.T) {
	input := "hello\x00world\x01test\x7f"
	got := sanitizeUntrusted(input)
	if got != "helloworldtest" {
		t.Errorf("expected control chars stripped, got %q", got)
	}
}

func TestSanitizeUntrusted_StripsFormatChars(t *testing.T) {
	// Zero-width space (U+200B), BOM (U+FEFF), Bi-di override (U+202E)
	input := "hello\u200Bworld\uFEFF\u202Etest"
	got := sanitizeUntrusted(input)
	if got != "helloworldtest" {
		t.Errorf("expected format chars stripped, got %q (len %d)", got, len(got))
	}
}

func TestSanitizeUntrusted_ConvertsLineSeparators(t *testing.T) {
	input := "line1\u2028line2\u2029line3"
	got := sanitizeUntrusted(input)
	if got != "line1\nline2\nline3" {
		t.Errorf("expected line separators converted to \\n, got %q", got)
	}
}

func TestSanitizeUntrusted_PreservesNewlineTabCR(t *testing.T) {
	input := "a\nb\rc\td"
	got := sanitizeUntrusted(input)
	if got != input {
		t.Errorf("should preserve \\n \\r \\t, got %q", got)
	}
}

func TestWrapUntrusted(t *testing.T) {
	input := "hello <script>alert('xss')</script>"
	got := wrapUntrusted(input)
	if !strings.HasPrefix(got, "<untrusted-text>") {
		t.Error("missing opening tag")
	}
	if !strings.HasSuffix(got, "\n</untrusted-text>") {
		t.Error("missing closing tag")
	}
	if strings.Contains(got, "<script>") {
		t.Error("< should be escaped")
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Errorf("< > not escaped properly: %s", got)
	}
}

func TestWrapUntrusted_AlsoSanitizes(t *testing.T) {
	input := "clean\x00dirty"
	got := wrapUntrusted(input)
	if strings.Contains(got, "\x00") {
		t.Error("control chars should be stripped before wrapping")
	}
}

func TestUnicodeCategory_ControlChars(t *testing.T) {
	tests := []struct {
		r    rune
		want string
	}{
		{0x00, "Cc"},
		{0x1F, "Cc"},
		{0x7F, "Cc"},
		{0x9F, "Cc"},
		{'A', ""},
		{'中', ""},
	}
	for _, tt := range tests {
		got := unicodeCategory(tt.r)
		if got != tt.want {
			t.Errorf("unicodeCategory(%U) = %q, want %q", tt.r, got, tt.want)
		}
	}
}

func TestUnicodeCategory_FormatChars(t *testing.T) {
	tests := []rune{0xAD, 0x200B, 0x200C, 0x200D, 0x200E, 0x200F, 0x202A, 0x2060, 0xFEFF}
	for _, r := range tests {
		got := unicodeCategory(r)
		if got != "Cf" {
			t.Errorf("unicodeCategory(%U) = %q, want 'Cf'", r, got)
		}
	}
}

func TestLoadFileWithBudget_SmallFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "small.md")
	os.WriteFile(f, []byte("hello world"), 0644)

	content, ok := loadFileWithBudget(f, 1000)
	if !ok {
		t.Error("should return ok")
	}
	if content != "hello world" {
		t.Errorf("content = %q", content)
	}
}

func TestLoadFileWithBudget_Truncation(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "big.md")
	// Create content with newlines
	lines := ""
	for i := 0; i < 100; i++ {
		lines += "line content here\n"
	}
	os.WriteFile(f, []byte(lines), 0644)

	content, ok := loadFileWithBudget(f, 50)
	if !ok {
		t.Error("should return ok even when truncated")
	}
	if !strings.Contains(content, "⚠️ [file truncated") {
		t.Errorf("truncated content should have warning, got: %s", content[:min(len(content), 200)])
	}
	if len(content) > 200 { // 50 chars + warning
		t.Errorf("content too long after truncation: %d", len(content))
	}
}

func TestLoadFileWithBudget_NonExistent(t *testing.T) {
	_, ok := loadFileWithBudget("/nonexistent/file", 1000)
	if ok {
		t.Error("should return not ok for nonexistent file")
	}
}

func TestEstimateTokens_Empty(t *testing.T) {
	if estimateTokens("") != 0 {
		t.Error("empty string should be 0 tokens")
	}
}

func TestEstimateTokens_English(t *testing.T) {
	// ~400 ASCII chars → ~100 tokens
	text := strings.Repeat("abcd", 100)
	tokens := estimateTokens(text)
	if tokens < 80 || tokens > 120 {
		t.Errorf("400 ASCII chars: expected ~100 tokens, got %d", tokens)
	}
}

func TestEstimateTokens_Chinese(t *testing.T) {
	// 100 Chinese chars → ~66 tokens (100 * 2/3)
	text := strings.Repeat("中", 100)
	tokens := estimateTokens(text)
	if tokens < 50 || tokens > 80 {
		t.Errorf("100 Chinese chars: expected ~66 tokens, got %d", tokens)
	}
}

func TestEstimateTokens_Mixed(t *testing.T) {
	text := "Hello World 你好世界"
	tokens := estimateTokens(text)
	if tokens < 3 || tokens > 10 {
		t.Errorf("mixed text: expected 3-10 tokens, got %d", tokens)
	}
}

func TestWritePrompt_SmallPrompt(t *testing.T) {
	origOut := promptOut
	dir := t.TempDir()
	promptOut = filepath.Join(dir, "prompt.md")
	defer func() { promptOut = origOut }()

	content := "small test prompt"
	writePrompt(buildPromptResult{content: content})

	data, err := os.ReadFile(promptOut)
	if err != nil {
		t.Fatalf("read prompt file: %v", err)
	}
	if string(data) != content {
		t.Errorf("written content mismatch: %q", data)
	}
}

func TestWritePrompt_LargePrompt(t *testing.T) {
	origOut := promptOut
	dir := t.TempDir()
	promptOut = filepath.Join(dir, "prompt.md")
	defer func() { promptOut = origOut }()

	// Create a prompt that exceeds the token limit
	// 100k tokens ~= 250k ASCII chars
	content := strings.Repeat("test content here ", 20000) // ~360k chars
	writePrompt(buildPromptResult{content: content})

	// Should still write the file (just warns on stderr)
	if _, err := os.Stat(promptOut); err != nil {
		t.Error("prompt file should be written even when over limit")
	}
}

func TestBuildPrompt_ContainsTelegramSection(t *testing.T) {
	requireWorkspace(t)
	result := buildPrompt()
	if !strings.Contains(result.content, "Telegram") {
		t.Error("prompt should mention Telegram")
	}
}

func TestBuildPrompt_ContainsStartupProtocol(t *testing.T) {
	requireWorkspace(t)
	result := buildPrompt()
	// Check for either custom BOOT.md content or default boot protocol
	if !strings.Contains(result.content, "Boot Protocol") && !strings.Contains(result.content, "启动协议") {
		t.Error("prompt missing startup protocol")
	}
}

func TestProfileForMode(t *testing.T) {
	tests := []struct {
		mode          string
		wantSoulCount int
		wantSkills    bool
		wantTelegram  bool
		wantHeartbeat bool
		wantBudget    int
	}{
		{"interactive", 5, true, true, false, 15000},
		{"heartbeat", 3, false, false, true, 8000},
		{"cron", 3, false, true, true, 15000},
		{"evolve", 5, true, false, false, 10000},
		{"", 5, true, true, false, 15000},       // default
		{"server", 5, true, true, false, 15000},  // default
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			p := profileForMode(tt.mode)
			if len(p.SoulFiles) != tt.wantSoulCount {
				t.Errorf("SoulFiles count = %d, want %d (files: %v)", len(p.SoulFiles), tt.wantSoulCount, p.SoulFiles)
			}
			if p.IncludeSkills != tt.wantSkills {
				t.Errorf("IncludeSkills = %v, want %v", p.IncludeSkills, tt.wantSkills)
			}
			if p.IncludeTelegram != tt.wantTelegram {
				t.Errorf("IncludeTelegram = %v, want %v", p.IncludeTelegram, tt.wantTelegram)
			}
			if p.IncludeHeartbeat != tt.wantHeartbeat {
				t.Errorf("IncludeHeartbeat = %v, want %v", p.IncludeHeartbeat, tt.wantHeartbeat)
			}
			if p.DailyNoteBudget != tt.wantBudget {
				t.Errorf("DailyNoteBudget = %d, want %d", p.DailyNoteBudget, tt.wantBudget)
			}
			// All profiles should include feedback, daily notes, CC sessions, and projects
			if !p.IncludeFeedback {
				t.Error("IncludeFeedback should be true for all modes")
			}
			if !p.IncludeDailyNotes {
				t.Error("IncludeDailyNotes should be true for all modes")
			}
			if !p.IncludeCCSessions {
				t.Error("IncludeCCSessions should be true for all modes")
			}
			if !p.IncludeProjects {
				t.Error("IncludeProjects should be true for all modes")
			}
		})
	}
}

func TestBuildPrompt_HeartbeatMode(t *testing.T) {
	requireWorkspace(t)
	origMode := currentMode
	currentMode = "heartbeat"
	defer func() { currentMode = origMode }()

	result := buildPrompt()
	// Heartbeat should NOT include SOUL.md or TOOLS.md
	if strings.Contains(result.content, "# === SOUL.md ===") {
		t.Error("heartbeat prompt should not include SOUL.md")
	}
	if strings.Contains(result.content, "# === TOOLS.md ===") {
		t.Error("heartbeat prompt should not include TOOLS.md")
	}
	// Should include IDENTITY.md and AGENTS.md
	if !strings.Contains(result.content, "# === IDENTITY.md ===") {
		t.Error("heartbeat prompt should include IDENTITY.md")
	}
	// Should NOT include Skills section
	if strings.Contains(result.content, "# === Skills ===") {
		t.Error("heartbeat prompt should not include Skills")
	}
	// Should NOT include Telegram
	if strings.Contains(result.content, "# === Telegram current conversation") {
		t.Error("heartbeat prompt should not include Telegram context")
	}
}

func TestBuildPrompt_HasReasonableSize(t *testing.T) {
	requireWorkspace(t)
	result := buildPrompt()
	tokens := estimateTokens(result.content)
	t.Logf("prompt: %d chars, ~%d tokens", len(result.content), tokens)
	if tokens > promptTokenLimit {
		t.Errorf("prompt exceeds token limit: %d > %d", tokens, promptTokenLimit)
	}
	if tokens < 1000 {
		t.Errorf("prompt suspiciously small: %d tokens", tokens)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
