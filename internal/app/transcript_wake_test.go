package app

import (
	"os"
	"path/filepath"
	"testing"
)

// writeJSONL helper writes a fake Claude Code session transcript into the
// temporary claude projects dir where findSessionJSONL expects to find it.
// Returns the ccID used in the filename so the caller can pass it to
// transcriptNeedsWake.
func writeJSONL(t *testing.T, claudeDir, name, body string) string {
	t.Helper()
	projectDir := filepath.Join(claudeDir, "projects", "-fake-project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(projectDir, name+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return name
}

// swapClaudeConfigDir points the package-global claudeConfigDir at tmp for
// the duration of the test.
func swapClaudeConfigDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	orig := claudeConfigDir
	claudeConfigDir = tmp
	t.Cleanup(func() { claudeConfigDir = orig })
	return tmp
}

func TestTranscriptNeedsWake_EndTurnNeedsWake(t *testing.T) {
	dir := swapClaudeConfigDir(t)
	body := `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}}
`
	id := writeJSONL(t, dir, "end-turn-session", body)
	if !transcriptNeedsWake(id) {
		t.Fatal("expected wake required for end_turn-closed transcript")
	}
}

func TestTranscriptNeedsWake_MaxTokensNeedsWake(t *testing.T) {
	dir := swapClaudeConfigDir(t)
	body := `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"cut off..."}],"stop_reason":"max_tokens"}}
`
	id := writeJSONL(t, dir, "max-tokens-session", body)
	if !transcriptNeedsWake(id) {
		t.Fatal("expected wake required for max_tokens-closed transcript")
	}
}

func TestTranscriptNeedsWake_PendingUserNoWake(t *testing.T) {
	dir := swapClaudeConfigDir(t)
	// Last real turn is a user message — CC will process it on resume.
	body := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}}
{"type":"user","message":{"role":"user","content":"another question"}}
`
	id := writeJSONL(t, dir, "pending-user-session", body)
	if transcriptNeedsWake(id) {
		t.Fatal("unexpected wake: pending user turn should be processed by CC naturally")
	}
}

func TestTranscriptNeedsWake_ToolUseNoWake(t *testing.T) {
	dir := swapClaudeConfigDir(t)
	// stop_reason=tool_use means CC expects a tool_result user turn next,
	// not a text ping. Auto-wake would be semantically wrong.
	body := `{"type":"user","message":{"role":"user","content":"list files"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"ls"}],"stop_reason":"tool_use"}}
`
	id := writeJSONL(t, dir, "tool-use-session", body)
	if transcriptNeedsWake(id) {
		t.Fatal("unexpected wake: tool_use transcripts must not receive text wake pings")
	}
}

func TestTranscriptNeedsWake_EmptyTranscriptNoWake(t *testing.T) {
	dir := swapClaudeConfigDir(t)
	id := writeJSONL(t, dir, "empty-session", "")
	if transcriptNeedsWake(id) {
		t.Fatal("unexpected wake: empty transcript should let CC wait for first input")
	}
}

func TestTranscriptNeedsWake_MetadataOnlyNoWake(t *testing.T) {
	dir := swapClaudeConfigDir(t)
	// Only metadata events, no real user/assistant turns. CC will wait for
	// first input like a fresh session.
	body := `{"type":"system","subtype":"init","model":"claude-opus-4-7"}
{"type":"last-prompt","lastPrompt":"something","sessionId":"metadata-only"}
`
	id := writeJSONL(t, dir, "metadata-only-session", body)
	if transcriptNeedsWake(id) {
		t.Fatal("unexpected wake: metadata-only transcripts should not trigger auto-wake")
	}
}

func TestTranscriptNeedsWake_MissingJSONLNoWake(t *testing.T) {
	swapClaudeConfigDir(t)
	if transcriptNeedsWake("does-not-exist-anywhere") {
		t.Fatal("unexpected wake: missing JSONL should preserve pre-existing behavior (no wake)")
	}
}

func TestTranscriptNeedsWake_EmptyCCIDNoWake(t *testing.T) {
	if transcriptNeedsWake("") {
		t.Fatal("unexpected wake: empty ccID must short-circuit to false")
	}
}

func TestTranscriptNeedsWake_SkipsNonConversationalRecords(t *testing.T) {
	dir := swapClaudeConfigDir(t)
	// Real turn is user, followed by non-conversational records (last-prompt,
	// system). The helper must ignore the trailing metadata and look at the
	// last real turn (user → no wake).
	body := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}}
{"type":"user","message":{"role":"user","content":"thanks"}}
{"type":"last-prompt","lastPrompt":"thanks","sessionId":"metadata-tail"}
{"type":"summary","summary":"a conversation"}
`
	id := writeJSONL(t, dir, "metadata-tail-session", body)
	if transcriptNeedsWake(id) {
		t.Fatal("unexpected wake: trailing metadata records must not mask the last real user turn")
	}
}

func TestTranscriptNeedsWake_EmptyStopReasonNoWake(t *testing.T) {
	dir := swapClaudeConfigDir(t)
	// CC frequently writes JSONL with an empty stop_reason on normal
	// assistant turns (see provider_gemini.go:1674 — the provider emits
	// a real finishReason but CC's JSONL records it as ""). Previously
	// this path returned "needs wake" as a safer default, but that made
	// the wake fire on nearly every resume, injecting a spurious "."
	// and polluting the transcript. We now require an explicit completion
	// marker (end_turn / max_tokens / stop_sequence) to trigger wake.
	body := `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}
`
	id := writeJSONL(t, dir, "empty-stop-reason-session", body)
	if transcriptNeedsWake(id) {
		t.Fatal("unexpected wake: empty stop_reason is ambiguous (CC often omits it); must not auto-inject a wake ping")
	}
}

func TestTranscriptNeedsWake_StopSequenceWakes(t *testing.T) {
	dir := swapClaudeConfigDir(t)
	body := `{"type":"user","message":{"role":"user","content":"count"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"1 2"}],"stop_reason":"stop_sequence"}}
`
	id := writeJSONL(t, dir, "stop-sequence-session", body)
	if !transcriptNeedsWake(id) {
		t.Fatal("expected wake for stop_sequence-closed transcript")
	}
}

func TestTranscriptNeedsWake_UnknownStopReasonNoWake(t *testing.T) {
	dir := swapClaudeConfigDir(t)
	// Any value outside the explicit whitelist (end_turn / max_tokens /
	// stop_sequence) is treated as "not confidently closed" → don't wake.
	body := `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"partial"}],"stop_reason":"refusal"}}
`
	id := writeJSONL(t, dir, "unknown-stop-reason-session", body)
	if transcriptNeedsWake(id) {
		t.Fatal("unexpected wake: unknown stop_reason must not trigger auto-wake")
	}
}
