package app

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Soul Session lifecycle: daily/trip/manual sessions that persist across heartbeats.
// Instead of spawning a new Claude session each heartbeat, we resume the active one.

const soulSessionSchema = `CREATE TABLE IF NOT EXISTS soul_sessions (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	agent_id          TEXT NOT NULL DEFAULT 'main',
	type              TEXT NOT NULL DEFAULT 'daily',
	status            TEXT NOT NULL DEFAULT 'running',
	claude_session_id TEXT NOT NULL DEFAULT '',
	budget_limit_usd  REAL NOT NULL DEFAULT 2.0,
	budget_used_usd   REAL NOT NULL DEFAULT 0,
	rounds            INTEGER NOT NULL DEFAULT 0,
	context           TEXT NOT NULL DEFAULT '',
	started_at        TEXT NOT NULL,
	updated_at        TEXT NOT NULL,
	ended_at          TEXT
)`

const soulSessionMigrationRounds = `ALTER TABLE soul_sessions ADD COLUMN rounds INTEGER NOT NULL DEFAULT 0`

// ensureSoulSessionTable adds the soul_sessions table if it doesn't exist.
// Called from openDB alongside other schemas.
func ensureSoulSessionTable(db *sql.DB) error {
	if _, err := db.Exec(soulSessionSchema); err != nil {
		return err
	}
	// Migration: add rounds column if missing (idempotent)
	db.Exec(soulSessionMigrationRounds) // ignore error = column already exists
	return nil
}

// getActiveSoulSession returns the claude_session_id for an active (running/suspended) soul session.
// Returns "" if no active session exists.
func getActiveSoulSession(agent string) string {
	db, err := openDB()
	if err != nil {
		return ""
	}
	defer db.Close()

	var claudeSID string
	err = db.QueryRow(
		`SELECT claude_session_id FROM soul_sessions
		 WHERE agent_id = ? AND status IN ('running', 'suspended') AND claude_session_id != ''
		 ORDER BY updated_at DESC LIMIT 1`, agent).Scan(&claudeSID)
	if err != nil {
		return ""
	}
	return claudeSID
}

// getActiveSoulSessionID returns the soul_session row ID for an active session.
func getActiveSoulSessionID(agent string) int64 {
	db, err := openDB()
	if err != nil {
		return 0
	}
	defer db.Close()

	var id int64
	db.QueryRow(
		`SELECT id FROM soul_sessions
		 WHERE agent_id = ? AND status IN ('running', 'suspended')
		 ORDER BY updated_at DESC LIMIT 1`, agent).Scan(&id)
	return id
}

// createSoulSession creates a new soul session and returns its ID.
func createSoulSession(agent, sessionType string, budgetLimit float64) int64 {
	db, err := openDB()
	if err != nil {
		return 0
	}
	defer db.Close()

	now := time.Now().Format(time.RFC3339)
	res, err := db.Exec(
		`INSERT INTO soul_sessions (agent_id, type, status, budget_limit_usd, started_at, updated_at)
		 VALUES (?, ?, 'running', ?, ?, ?)`,
		agent, sessionType, budgetLimit, now, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] soul session create failed: %v\n", appName, err)
		return 0
	}
	id, _ := res.LastInsertId()
	fmt.Fprintf(os.Stderr, "[%s] created soul session #%d (type=%s, budget=$%.2f)\n", appName, id, sessionType, budgetLimit)
	return id
}

// linkSoulSession links a Claude session ID to an existing soul session.
func linkSoulSession(soulSessionID int64, claudeSessionID string) {
	db, err := openDB()
	if err != nil {
		return
	}
	defer db.Close()

	now := time.Now().Format(time.RFC3339)
	db.Exec(`UPDATE soul_sessions SET claude_session_id = ?, updated_at = ? WHERE id = ?`,
		claudeSessionID, now, soulSessionID)
	fmt.Fprintf(os.Stderr, "[%s] linked soul session #%d → claude %s\n", appName, soulSessionID, shortID(claudeSessionID))
}

// touchSoulSession updates the updated_at timestamp.
func touchSoulSession(agent string) {
	db, err := openDB()
	if err != nil {
		return
	}
	defer db.Close()

	now := time.Now().Format(time.RFC3339)
	db.Exec(`UPDATE soul_sessions SET updated_at = ? WHERE agent_id = ? AND status IN ('running', 'suspended')`, now, agent)
}

// updateSoulSessionBudget updates budget_used_usd from proxy cost aggregation.
func updateSoulSessionBudget(agent string) {
	db, err := openDB()
	if err != nil {
		return
	}
	defer db.Close()

	var id int64
	var claudeSID string
	var budgetLimit float64
	err = db.QueryRow(
		`SELECT id, claude_session_id, budget_limit_usd FROM soul_sessions
		 WHERE agent_id = ? AND status IN ('running', 'suspended')
		 ORDER BY updated_at DESC LIMIT 1`, agent).Scan(&id, &claudeSID, &budgetLimit)
	if err != nil || claudeSID == "" {
		return
	}

	// Try to get cost from proxy DB (server_proxy.go's getSessionCostFromProxy)
	cost := getProxyCostForSession(claudeSID)
	if cost <= 0 {
		return
	}

	now := time.Now().Format(time.RFC3339)
	db.Exec(`UPDATE soul_sessions SET budget_used_usd = ?, updated_at = ? WHERE id = ?`, cost, now, id)

	if cost >= budgetLimit {
		fmt.Fprintf(os.Stderr, "[%s] soul session #%d budget exceeded ($%.2f >= $%.2f), ending session\n",
			appName, id, cost, budgetLimit)
		db.Exec(`UPDATE soul_sessions SET status = 'ended', ended_at = ? WHERE id = ?`, now, id)
	}
}

// endSoulSession marks a soul session as ended.
func endSoulSession(agent, reason string) {
	db, err := openDB()
	if err != nil {
		return
	}
	defer db.Close()

	now := time.Now().Format(time.RFC3339)
	ctx := reason
	db.Exec(`UPDATE soul_sessions SET status = 'ended', context = ?, ended_at = ?, updated_at = ? WHERE agent_id = ? AND status IN ('running', 'suspended')`,
		ctx, now, now, agent)
	fmt.Fprintf(os.Stderr, "[%s] ended soul session for %s: %s\n", appName, agent, reason)
}

// endStaleSoulSessions ends sessions that haven't been updated in over 24h.
func endStaleSoulSessions() {
	db, err := openDB()
	if err != nil {
		return
	}
	defer db.Close()

	cutoff := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	res, err := db.Exec(`UPDATE soul_sessions SET status = 'ended', context = 'auto-expired: 24h inactive', ended_at = datetime('now'), updated_at = datetime('now')
		WHERE status IN ('running', 'suspended') AND updated_at < ?`, cutoff)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		fmt.Fprintf(os.Stderr, "[%s] expired %d stale soul sessions\n", appName, n)
	}
}

// detectNewSession finds the most recently created JSONL in the workspace project dir.
// Called after runClaude to capture the session ID of a newly created session.
func detectNewSession(before time.Time) string {
	// Claude Code stores sessions in ~/.claude/projects/<encoded-workspace>/*.jsonl
	encodedWS := encodeProjectPath(workspace)
	projDir := filepath.Join(claudeConfigDir, "projects", encodedWS)

	entries, err := os.ReadDir(projDir)
	if err != nil {
		return ""
	}

	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Only consider files created after `before`
		if info.ModTime().After(before) && info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = strings.TrimSuffix(e.Name(), ".jsonl")
		}
	}
	return newest
}

// encodeProjectPath encodes a workspace path the same way Claude Code does.
// /Users/alice/.openclaw/workspace → -Users-alice--openclaw-workspace
func encodeProjectPath(path string) string {
	// Replace / with -
	s := strings.ReplaceAll(path, "/", "-")
	// Replace /. (dot after slash) with // which then becomes --
	// Actually Claude Code: leading dot in segment → double the separator
	// .openclaw → -openclaw but the dot is encoded as extra -
	// Let's just do the simple replacement
	s = strings.ReplaceAll(s, "-.", "--")
	return s
}

// incrementSoulSessionRounds bumps the rounds counter for the active soul session.
func incrementSoulSessionRounds(agent string) {
	db, err := openDB()
	if err != nil {
		return
	}
	defer db.Close()

	now := time.Now().Format(time.RFC3339)
	db.Exec(`UPDATE soul_sessions SET rounds = rounds + 1, updated_at = ?
		WHERE agent_id = ? AND status IN ('running', 'suspended')`, now, agent)
}

// shouldCompactSoulSession returns true if the active soul session has exceeded maxRounds.
// Returns false if maxRounds <= 0 (disabled) or no active session.
func shouldCompactSoulSession(agent string, maxRounds int) bool {
	if maxRounds <= 0 {
		return false
	}
	db, err := openDB()
	if err != nil {
		return false
	}
	defer db.Close()

	var rounds int
	err = db.QueryRow(
		`SELECT rounds FROM soul_sessions
		 WHERE agent_id = ? AND status IN ('running', 'suspended')
		 ORDER BY updated_at DESC LIMIT 1`, agent).Scan(&rounds)
	if err != nil {
		return false
	}
	return rounds >= maxRounds
}

// getProxyCostForSession aggregates cost from proxy_requests table for a session.
func getProxyCostForSession(claudeSessionID string) float64 {
	// Try the proxy DB path
	proxyDB := filepath.Join(appDir, "proxy.db")
	if _, err := os.Stat(proxyDB); err != nil {
		return 0
	}

	db, err := sql.Open("sqlite", proxyDB)
	if err != nil {
		return 0
	}
	defer db.Close()

	var cost float64
	db.QueryRow(`SELECT COALESCE(SUM(cost_usd), 0) FROM proxy_requests WHERE session_id = ?`, claudeSessionID).Scan(&cost)
	return cost
}
