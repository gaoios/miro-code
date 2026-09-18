package app

// server_content_hook.go — PostAssistantMsg: server-side content matching hook.
//
// CC's hook system has no event for matching assistant output. The weiran
// server sits in the message pipeline (bridgeStdout) and can see all assistant
// text segments in real time. This file implements:
//
//   1. Detect keywords in assistant text (mid-turn, before tool calls)
//   2. Write matched injections to a shared SQLite table
//   3. The tool-hook binary (forked per PreToolUse) drains the table and
//      appends injections to additionalContext — same turn, immediate effect
//
// Rules live in the same tool-hooks.yaml with `events: [PostAssistantMsg]`.
// Matching uses `match_prompt` (regex on text) and `skip_input` (exclusion).

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// HookEventPostAssistantMsg is defined in tool_hook.go alongside other event constants.

// ── Config cache (mtime-based hot-reload) ──

type contentHookConfigCache struct {
	mu      sync.Mutex
	path    string
	mtime   time.Time
	config  *ToolHookConfig
}

var chConfigCache contentHookConfigCache

// loadContentHookConfig returns the parsed tool-hooks.yaml, using a cached
// copy if the file hasn't changed (mtime check). Thread-safe.
func loadContentHookConfig() *ToolHookConfig {
	chConfigCache.mu.Lock()
	defer chConfigCache.mu.Unlock()

	path := chConfigCache.path
	if path == "" {
		path = defaultToolHookConfigPath()
		chConfigCache.path = path
	}

	info, err := os.Stat(path)
	if err != nil {
		return &ToolHookConfig{Budget: 1500}
	}

	if chConfigCache.config != nil && info.ModTime().Equal(chConfigCache.mtime) {
		return chConfigCache.config
	}

	cfg, err := loadToolHookConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] content-hook: load config: %v\n", appName, err)
		return &ToolHookConfig{Budget: 1500}
	}

	chConfigCache.mtime = info.ModTime()
	chConfigCache.config = cfg
	return cfg
}

// ── DB table for cross-process pending injections ──

const contentHookPendingSchema = `CREATE TABLE IF NOT EXISTS content_hook_pending (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL,
	rule_id    TEXT NOT NULL,
	injection  TEXT NOT NULL,
	created_at TEXT NOT NULL,
	consumed   INTEGER NOT NULL DEFAULT 0
)`

const contentHookPendingIndex = `CREATE INDEX IF NOT EXISTS idx_chp_session ON content_hook_pending(session_id, consumed)`

// ensureContentHookTable creates the pending table if it doesn't exist.
func ensureContentHookTable(db *sql.DB) {
	if db == nil {
		return
	}
	db.Exec(contentHookPendingSchema)
	db.Exec(contentHookPendingIndex)
}

// ── Evaluation (server-side, in-process) ──

// evalPostAssistantMsg evaluates PostAssistantMsg rules against a completed
// assistant text segment. Matched rules' inject text is written to the
// content_hook_pending table for the tool-hook binary to consume.
//
// Called from the bridgeStdout handler when a tool_use event arrives
// (meaning the preceding text segment is complete and mid-turn).
func evalPostAssistantMsg(db *sql.DB, sessionID, segmentText string) {
	if sessionID == "" || strings.TrimSpace(segmentText) == "" {
		return
	}

	cfg := loadContentHookConfig()
	if cfg == nil || len(cfg.Rules) == 0 {
		return
	}

	budgetRemaining := cfg.Budget
	now := time.Now()

	for _, rule := range cfg.Rules {
		if rule.Disabled || rule.ID == "" {
			continue
		}
		if isTempDisabled(&rule) {
			continue
		}
		if !rule.eventMatches(HookEventPostAssistantMsg) {
			continue
		}
		// Match against assistant text using match_prompt regex
		if !matchesPromptRegex(segmentText, rule.MatchPrompt) {
			continue
		}
		// Skip exclusion
		if len(rule.SkipInput) > 0 && matchesPromptRegex(segmentText, rule.SkipInput) {
			continue
		}
		// Dedupe: per_session (no path concept for content hooks)
		if db != nil && isDedupedSessionScoped(db, rule, sessionID, HookEventPostAssistantMsg) {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  now.Format(time.RFC3339),
				SessionID:  sessionID,
				EventName:  HookEventPostAssistantMsg,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: "dedupe",
			})
			continue
		}

		body := strings.TrimSpace(rule.Inject)
		if len(body) > rule.Budget {
			body = body[:rule.Budget]
		}
		if len(body) > budgetRemaining {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  now.Format(time.RFC3339),
				SessionID:  sessionID,
				EventName:  HookEventPostAssistantMsg,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: "budget",
			})
			continue
		}

		injection := fmt.Sprintf("[rule:%s] %s", rule.ID, body)
		budgetRemaining -= len(body)

		// Write to pending table for tool-hook binary to consume
		writeContentHookPending(db, sessionID, rule.ID, injection)

		// Audit success
		writeToolHookAudit(db, toolHookAuditRow{
			Timestamp:     now.Format(time.RFC3339),
			SessionID:     sessionID,
			EventName:     HookEventPostAssistantMsg,
			RuleID:        rule.ID,
			Injected:      true,
			InjectionSize: len(body),
			BudgetUsed:    cfg.Budget - budgetRemaining,
		})
	}
}

// writeContentHookPending inserts a pending injection row.
func writeContentHookPending(db *sql.DB, sessionID, ruleID, injection string) {
	if db == nil {
		return
	}
	_, err := db.Exec(`INSERT INTO content_hook_pending (session_id, rule_id, injection, created_at)
		VALUES (?, ?, ?, ?)`, sessionID, ruleID, injection, time.Now().Format(time.RFC3339))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] content-hook: write pending: %v\n", appName, err)
	}
}

// ── Drain (called by tool-hook binary side) ──

// drainContentHookPending reads and consumes all pending injections for a
// session. Called by the tool-hook binary in runPreToolUseHook/runPostToolUseHook.
// Returns formatted injection strings ready to prepend to additionalContext.
func drainContentHookPending(db *sql.DB, sessionID string) []string {
	if db == nil || sessionID == "" {
		return nil
	}

	// Atomic select+delete in a transaction to prevent duplicate drain
	// if two hook processes race on the same session.
	tx, err := db.Begin()
	if err != nil {
		return nil
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id, injection FROM content_hook_pending
		WHERE session_id = ? AND consumed = 0 ORDER BY id`, sessionID)
	if err != nil {
		return nil
	}

	var ids []int64
	var injections []string
	for rows.Next() {
		var id int64
		var inj string
		if rows.Scan(&id, &inj) == nil {
			ids = append(ids, id)
			injections = append(injections, inj)
		}
	}
	rows.Close()

	if len(ids) == 0 {
		return nil
	}

	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	tx.Exec("DELETE FROM content_hook_pending WHERE id IN ("+strings.Join(placeholders, ",")+
		")", args...)
	tx.Commit()

	return injections
}

// gcContentHookPending cleans up stale rows. Called periodically.
// Rows are normally deleted on drain; this catches leftovers from crashed
// sessions or turns that ended without any tool call to trigger drain.
func gcContentHookPending(db *sql.DB) {
	if db == nil {
		return
	}
	// All rows older than 5 minutes are stale (drain happens within seconds).
	cutoff := time.Now().Add(-5 * time.Minute).Format(time.RFC3339)
	db.Exec("DELETE FROM content_hook_pending WHERE created_at < ?", cutoff)
}
