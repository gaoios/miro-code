package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ── Codex session message store (Round 5+) ──
//
// Codex's app-server doesn't write a transcript file we can re-read like the
// Claude Code CLI does (~/.claude/projects/<proj>/<session>.jsonl). Without a
// persistence layer the Web UI's /api/history/{id}/messages endpoint, the
// FTS index, the auto-rename pipeline, and resume/IPC peers all see codex
// sessions as empty — which is exactly what surfaced as "session 0f1a5670
// won't start": handshake completed, prompt was sent, but num_turns stayed
// at 0 and there was no message to render.
//
// To stay backend-agnostic above the bridge, we mirror codex's thread/turn
// event stream into a JSONL file that follows the same schema as Claude
// Code's transcript. Every existing reader (parseSessionMessages, FTS
// indexer, rename worker) is reused as-is — we only add a writer here.
//
// File layout
// -----------
// Path:  <appDir>/codex/<thread_id>.jsonl
// One JSON object per line. Schema fields below match the subset that
// parseSessionMessages / parseSessionSubagents inspect:
//
//   {"type":"system","subtype":"init","timestamp":"…","sessionId":"…","model":"…"}
//   {"type":"user",    "timestamp":"…","message":{"role":"user",    "content":"text"}}
//   {"type":"assistant","timestamp":"…","message":{"role":"assistant","content":[{"type":"text","text":"…"}],"model":"…"}}
//
// Concurrency
// -----------
// codex bridge events arrive on a single goroutine but the user-message
// write happens on the request handler goroutine, so we serialize through a
// per-thread mutex. The map only grows for the lifetime of the process and
// codex sessions are short-lived; no GC needed.

var (
	codexJSONLMu    sync.Mutex
	codexJSONLLocks = map[string]*sync.Mutex{}
)

// codexJSONLLockFor returns a mutex scoped to one threadID so concurrent
// writers (sendMessage on the request goroutine, bridge handlers on the
// event goroutine) don't interleave bytes mid-line.
func codexJSONLLockFor(threadID string) *sync.Mutex {
	codexJSONLMu.Lock()
	defer codexJSONLMu.Unlock()
	if m, ok := codexJSONLLocks[threadID]; ok {
		return m
	}
	m := &sync.Mutex{}
	codexJSONLLocks[threadID] = m
	return m
}

// codexJSONLPath returns the absolute path for a codex thread's transcript.
// The directory is created lazily on first append. Returns empty string
// when threadID is empty (caller skips the write).
func codexJSONLPath(threadID string) string {
	if threadID == "" {
		return ""
	}
	return filepath.Join(appDir, "codex", threadID+".jsonl")
}

// appendCodexJSONL serializes obj as a single JSON line and appends it to
// the thread's transcript. Errors are logged to stderr (mirroring the rest
// of the codex bridge) but never propagated — losing a transcript line is
// strictly worse than silently dropping it, but better than crashing the
// session.
func appendCodexJSONL(threadID string, obj any) {
	path := codexJSONLPath(threadID)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] codex jsonl: mkdir %s: %v\n", appName, filepath.Dir(path), err)
		return
	}
	line, err := json.Marshal(obj)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] codex jsonl: marshal: %v\n", appName, err)
		return
	}
	mu := codexJSONLLockFor(threadID)
	mu.Lock()
	defer mu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] codex jsonl: open %s: %v\n", appName, path, err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] codex jsonl: write %s: %v\n", appName, path, err)
	}
}

// nowRFC3339 returns the timestamp string CC uses in transcripts so the
// Web UI's existing parser doesn't need to special-case codex.
func nowRFC3339() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// writeCodexSystemInit records a synthesized init line so readSessionMessages
// and readClaudeSessionName can recognize a codex transcript the same way
// they recognize a CC transcript. Idempotent guard at call sites — if the
// file is already non-empty the bridge skips this.
func writeCodexSystemInit(threadID, model, cwd string) {
	if threadID == "" {
		return
	}
	appendCodexJSONL(threadID, map[string]any{
		"type":      "system",
		"subtype":   "init",
		"timestamp": nowRFC3339(),
		"sessionId": threadID,
		"model":     model,
		"cwd":       cwd,
		"backend":   "codex",
	})
}

// writeCodexUserMessage records the user's turn. Called from sendMessage
// before turn/start so the line is on disk even if codex rejects the input.
func writeCodexUserMessage(threadID, content string) {
	if threadID == "" || content == "" {
		return
	}
	appendCodexJSONL(threadID, map[string]any{
		"parentUuid": nil,
		"uuid":       uuid.NewString(),
		"type":       "user",
		"timestamp":  nowRFC3339(),
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	})
}

// writeCodexAssistantMessage records a final assistant text item. Tool
// calls and reasoning items are written via writeCodexAssistantBlock so
// they share the same content-array shape parseSessionMessages expects.
func writeCodexAssistantMessage(threadID, model, text string) {
	if threadID == "" || text == "" {
		return
	}
	appendCodexJSONL(threadID, map[string]any{
		"uuid":      uuid.NewString(),
		"type":      "assistant",
		"timestamp": nowRFC3339(),
		"message": map[string]any{
			"role":  "assistant",
			"model": model,
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
		},
	})
}

// writeCodexAssistantBlock records a non-text assistant content block
// (tool_use / thinking / file_change). Schema follows CC so the existing
// subagent backfill keeps working without codex-specific code paths.
func writeCodexAssistantBlock(threadID, model string, block map[string]any) {
	if threadID == "" || len(block) == 0 {
		return
	}
	appendCodexJSONL(threadID, map[string]any{
		"uuid":      uuid.NewString(),
		"type":      "assistant",
		"timestamp": nowRFC3339(),
		"message": map[string]any{
			"role":    "assistant",
			"model":   model,
			"content": []map[string]any{block},
		},
	})
}

// maxCodexToolResultBytes caps tool_result content before persistence.
// Codex's file_change items hand back the raw JSON of the patch, which
// can balloon FTS index size and JSONL line length far past anything
// the Web UI would render (display already truncates at 4000 chars).
// 16KB is generous for human-readable command output and keeps the
// transcript file small.
const maxCodexToolResultBytes = 16 * 1024

// writeCodexToolResult records the tool_result so the Web UI can pair it
// with the tool_use block in the previous assistant line. Codex emits
// these as item/completed for tool_call items.
func writeCodexToolResult(threadID, toolUseID, content string, isError bool) {
	if threadID == "" || toolUseID == "" {
		return
	}
	if len(content) > maxCodexToolResultBytes {
		content = content[:maxCodexToolResultBytes] + "\n…[truncated]"
	}
	block := map[string]any{
		"type":        "tool_result",
		"tool_use_id": toolUseID,
		"content":     content,
	}
	if isError {
		block["is_error"] = true
	}
	appendCodexJSONL(threadID, map[string]any{
		"uuid":      uuid.NewString(),
		"type":      "user",
		"timestamp": nowRFC3339(),
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{block},
		},
	})
}
