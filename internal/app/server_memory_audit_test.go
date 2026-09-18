package app

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

func TestIsMemoryPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/Users/kiyor/.openclaw/workspace/memory/2026-04-12.md", true},
		{"/Users/kiyor/.openclaw/workspace/memory/topics/infrastructure.md", true},
		{"/Users/kiyor/.openclaw/workspace/MEMORY.md", true},
		{"/Users/kiyor/.openclaw/workspace/memory/evolve/micro/test.md", true},
		{"/Users/kiyor/.openclaw/workspace/SOUL.md", false},
		{"/Users/kiyor/.openclaw/workspace/projects/jira/main.go", false},
		{"/tmp/something.md", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isMemoryPath(tt.path); got != tt.want {
			t.Errorf("isMemoryPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestClassifyMemoryToolUse(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		input    string
		wantOp   string
		wantPath string
		wantOK   bool
	}{
		{
			name:     "Read memory file",
			toolName: "Read",
			input:    `{"file_path": "/Users/kiyor/.openclaw/workspace/memory/topics/infrastructure.md"}`,
			wantOp:   "recall",
			wantPath: "/Users/kiyor/.openclaw/workspace/memory/topics/infrastructure.md",
			wantOK:   true,
		},
		{
			name:     "Read non-memory file",
			toolName: "Read",
			input:    `{"file_path": "/Users/kiyor/.openclaw/workspace/SOUL.md"}`,
			wantOK:   false,
		},
		{
			name:     "Grep in memory dir",
			toolName: "Grep",
			input:    `{"path": "/Users/kiyor/.openclaw/workspace/memory/", "pattern": "infrastructure"}`,
			wantOp:   "search",
			wantPath: "/Users/kiyor/.openclaw/workspace/memory/",
			wantOK:   true,
		},
		{
			name:     "Write memory file",
			toolName: "Write",
			input:    `{"file_path": "/Users/kiyor/.openclaw/workspace/memory/2026-04-12.md", "content": "test"}`,
			wantOp:   "store",
			wantPath: "/Users/kiyor/.openclaw/workspace/memory/2026-04-12.md",
			wantOK:   true,
		},
		{
			name:     "Edit memory file",
			toolName: "Edit",
			input:    `{"file_path": "/Users/kiyor/.openclaw/workspace/memory/topics/kiyor.md", "old_string": "a", "new_string": "b"}`,
			wantOp:   "store",
			wantPath: "/Users/kiyor/.openclaw/workspace/memory/topics/kiyor.md",
			wantOK:   true,
		},
		{
			name:     "Glob memory dir",
			toolName: "Glob",
			input:    `{"pattern": "memory/topics/*.md", "path": "/Users/kiyor/.openclaw/workspace/memory/"}`,
			wantOp:   "list",
			wantOK:   true,
		},
		{
			name:     "Read MEMORY.md index",
			toolName: "Read",
			input:    `{"file_path": "/Users/kiyor/.openclaw/workspace/MEMORY.md"}`,
			wantOp:   "recall",
			wantPath: "/Users/kiyor/.openclaw/workspace/MEMORY.md",
			wantOK:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op, path, _, ok := classifyMemoryToolUse(tt.toolName, json.RawMessage(tt.input))
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && op != tt.wantOp {
				t.Errorf("op = %q, want %q", op, tt.wantOp)
			}
			if ok && tt.wantPath != "" && path != tt.wantPath {
				t.Errorf("path = %q, want %q", path, tt.wantPath)
			}
		})
	}
}

func TestExtractMemoryToolUse(t *testing.T) {
	// Simulate a tool_use SSE message with a Read targeting memory
	raw := json.RawMessage(`{
		"type": "assistant",
		"message": {
			"content": [
				{
					"type": "tool_use",
					"id": "toolu_abc123",
					"name": "Read",
					"input": {"file_path": "/Users/kiyor/.openclaw/workspace/memory/topics/infrastructure.md"}
				}
			]
		}
	}`)

	pending := extractMemoryToolUse(raw)
	if pending == nil {
		t.Fatal("expected non-nil pendingMemoryOp")
	}
	if pending.ToolUseID != "toolu_abc123" {
		t.Errorf("ToolUseID = %q, want toolu_abc123", pending.ToolUseID)
	}
	if pending.Operation != "recall" {
		t.Errorf("Operation = %q, want recall", pending.Operation)
	}
	if pending.ToolName != "Read" {
		t.Errorf("ToolName = %q, want Read", pending.ToolName)
	}
}

func TestExtractMemoryToolUse_NonMemory(t *testing.T) {
	raw := json.RawMessage(`{
		"type": "assistant",
		"message": {
			"content": [
				{
					"type": "tool_use",
					"id": "toolu_xyz",
					"name": "Read",
					"input": {"file_path": "/Users/kiyor/.openclaw/workspace/SOUL.md"}
				}
			]
		}
	}`)
	if pending := extractMemoryToolUse(raw); pending != nil {
		t.Errorf("expected nil for non-memory path, got %+v", pending)
	}
}

func TestMatchMemoryToolResult(t *testing.T) {
	pending := map[string]*pendingMemoryOp{
		"toolu_abc123": {
			ToolUseID: "toolu_abc123",
			Operation: "recall",
			ToolName:  "Read",
			Path:      "/Users/kiyor/.openclaw/workspace/memory/topics/infrastructure.md",
			StartTime: time.Now().Add(-50 * time.Millisecond),
		},
	}

	// Simulate a tool_result with content
	raw := json.RawMessage(`{
		"type": "user",
		"message": {
			"content": [
				{
					"type": "tool_result",
					"tool_use_id": "toolu_abc123",
					"content": "---\nname: infrastructure\n---\nMac mini M4 Pro..."
				}
			]
		}
	}`)

	entry := matchMemoryToolResult(raw, pending)
	if entry == nil {
		t.Fatal("expected non-nil audit entry")
	}
	if entry.Operation != "recall" {
		t.Errorf("Operation = %q, want recall", entry.Operation)
	}
	if !entry.Hit {
		t.Error("expected hit = true for non-empty content")
	}
	if entry.ResultSize == 0 {
		t.Error("expected non-zero result size")
	}
	if entry.LatencyMS < 0 {
		t.Error("expected non-negative latency")
	}
	// Pending should be consumed
	if len(pending) != 0 {
		t.Errorf("expected pending map to be empty, got %d", len(pending))
	}
}

func TestMatchMemoryToolResult_Miss(t *testing.T) {
	pending := map[string]*pendingMemoryOp{
		"toolu_miss": {
			ToolUseID: "toolu_miss",
			Operation: "recall",
			ToolName:  "Read",
			Path:      "/Users/kiyor/.openclaw/workspace/memory/topics/nonexistent.md",
			StartTime: time.Now(),
		},
	}

	raw := json.RawMessage(`{
		"type": "user",
		"message": {
			"content": [
				{
					"type": "tool_result",
					"tool_use_id": "toolu_miss",
					"content": "The path /Users/kiyor/.openclaw/workspace/memory/topics/nonexistent.md does not exist."
				}
			]
		}
	}`)

	entry := matchMemoryToolResult(raw, pending)
	if entry == nil {
		t.Fatal("expected non-nil audit entry")
	}
	if entry.Hit {
		t.Error("expected hit = false for 'does not exist' result")
	}
}

func TestLogAndQueryMemoryAudit(t *testing.T) {
	// In-memory SQLite
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec("PRAGMA journal_mode=WAL")

	// Create schema
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS memory_audit (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp    TEXT NOT NULL,
		session_id   TEXT NOT NULL DEFAULT '',
		session_name TEXT NOT NULL DEFAULT '',
		operation    TEXT NOT NULL,
		tool_name    TEXT NOT NULL DEFAULT '',
		path         TEXT DEFAULT '',
		query        TEXT DEFAULT '',
		latency_ms   INTEGER DEFAULT 0,
		hit          BOOLEAN DEFAULT 0,
		result_size  INTEGER DEFAULT 0
	)`)
	if err != nil {
		t.Fatal(err)
	}

	// Log some entries
	entries := []memoryAuditEntry{
		{Timestamp: time.Now(), SessionID: "sess1", SessionName: "test", Operation: "recall", ToolName: "Read", Path: "memory/topics/infra.md", Hit: true, ResultSize: 500, LatencyMS: 30},
		{Timestamp: time.Now(), SessionID: "sess1", SessionName: "test", Operation: "recall", ToolName: "Read", Path: "memory/topics/missing.md", Hit: false, ResultSize: 0, LatencyMS: 5},
		{Timestamp: time.Now(), SessionID: "sess1", SessionName: "test", Operation: "search", ToolName: "Grep", Path: "memory/", Query: "kubernetes", Hit: true, ResultSize: 1200, LatencyMS: 80},
		{Timestamp: time.Now(), SessionID: "sess2", SessionName: "hb", Operation: "store", ToolName: "Write", Path: "memory/2026-04-12.md", Hit: true, ResultSize: 200, LatencyMS: 10},
	}
	for _, e := range entries {
		logMemoryAudit(db, e)
	}

	// Query all
	results, err := queryMemoryAudit(db, 1, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 4 {
		t.Errorf("expected 4 results, got %d", len(results))
	}

	// Query by operation
	results, err = queryMemoryAudit(db, 1, "recall", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 recall results, got %d", len(results))
	}

	// Stats
	stats, err := queryMemoryAuditStats(db, 1)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalOps != 4 {
		t.Errorf("expected 4 total ops, got %d", stats.TotalOps)
	}
	recallStats, ok := stats.ByOperation["recall"]
	if !ok {
		t.Fatal("expected recall in stats")
	}
	if recallStats.Count != 2 {
		t.Errorf("recall count = %d, want 2", recallStats.Count)
	}
	if recallStats.Hits != 1 {
		t.Errorf("recall hits = %d, want 1", recallStats.Hits)
	}
	if recallStats.HitRate != 0.5 {
		t.Errorf("recall hit rate = %f, want 0.5", recallStats.HitRate)
	}
}
