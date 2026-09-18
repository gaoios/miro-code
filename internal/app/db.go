package app

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ── DB ──

func openDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open DB: %w", err)
	}
	// WAL mode: allows concurrent readers while writing, much better for multi-process access
	db.Exec("PRAGMA journal_mode=WAL")
	// Busy timeout: wait up to 5s for lock instead of failing immediately
	db.Exec("PRAGMA busy_timeout=5000")
	schemas := []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			path        TEXT PRIMARY KEY,
			size        INTEGER NOT NULL,
			hash        TEXT NOT NULL,
			summary     TEXT NOT NULL DEFAULT '',
			summary_seg TEXT NOT NULL DEFAULT '',
			updated_at  TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS patterns (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT NOT NULL UNIQUE,
			description TEXT NOT NULL DEFAULT '',
			example     TEXT NOT NULL DEFAULT '',
			sources     TEXT NOT NULL DEFAULT '[]',
			first_seen  TEXT NOT NULL,
			last_seen   TEXT NOT NULL,
			seen_count  INTEGER NOT NULL DEFAULT 1,
			status      TEXT NOT NULL DEFAULT 'candidate'
		)`,
		`CREATE TABLE IF NOT EXISTS pattern_feedback (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			pattern_id INTEGER NOT NULL REFERENCES patterns(id),
			outcome    TEXT NOT NULL,
			note       TEXT NOT NULL DEFAULT '',
			session    TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS session_agents (
			claude_session_id TEXT PRIMARY KEY,
			agent_id          TEXT NOT NULL DEFAULT 'main',
			agent_name        TEXT NOT NULL DEFAULT '',
			source            TEXT NOT NULL DEFAULT '',
			created_at        TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS spawns (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			agent       TEXT NOT NULL,
			agent_name  TEXT NOT NULL DEFAULT '',
			task        TEXT NOT NULL,
			session     TEXT NOT NULL DEFAULT '',
			pid         INTEGER NOT NULL DEFAULT 0,
			exit_code   INTEGER DEFAULT NULL,
			duration_s  REAL DEFAULT NULL,
			log_path    TEXT NOT NULL DEFAULT '',
			output_tail TEXT NOT NULL DEFAULT '',
			status      TEXT NOT NULL DEFAULT 'running',
			started_at  TEXT NOT NULL,
			finished_at TEXT DEFAULT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS memory_audit (
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
		)`,
		`CREATE INDEX IF NOT EXISTS idx_maudit_ts ON memory_audit(timestamp)`,
		`CREATE INDEX IF NOT EXISTS idx_maudit_op ON memory_audit(operation)`,
		`CREATE INDEX IF NOT EXISTS idx_maudit_sess ON memory_audit(session_id)`,
		// tool_hook_audit: every hook invocation (Read/Edit/Write/Grep/Prompt/...).
		// One row per hook call. Acts simultaneously as audit log and dedup state
		// (per_session/per_file dedup implemented via WHERE injected=1 lookups here).
		`CREATE TABLE IF NOT EXISTS tool_hook_audit (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp       TEXT NOT NULL,
			session_id      TEXT NOT NULL DEFAULT '',
			cwd             TEXT NOT NULL DEFAULT '',
			event_name      TEXT NOT NULL DEFAULT 'PreToolUse',
			tool_name       TEXT NOT NULL DEFAULT '',
			path            TEXT NOT NULL DEFAULT '',
			rule_id         TEXT NOT NULL DEFAULT '',
			injected        BOOLEAN NOT NULL DEFAULT 0,
			skip_reason     TEXT NOT NULL DEFAULT '',
			injection_size  INTEGER NOT NULL DEFAULT 0,
			budget_used     INTEGER NOT NULL DEFAULT 0,
			latency_ms      INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_thook_ts ON tool_hook_audit(timestamp)`,
		`CREATE INDEX IF NOT EXISTS idx_thook_sess ON tool_hook_audit(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_thook_rule ON tool_hook_audit(rule_id)`,
		`CREATE INDEX IF NOT EXISTS idx_thook_path ON tool_hook_audit(path)`,
		`CREATE INDEX IF NOT EXISTS idx_thook_tool ON tool_hook_audit(tool_name)`,
		// prompt_routing_audit: every buildPrompt invocation records which soul
		// mode was selected, what signals drove the decision, and the assembled
		// prompt size. Used to verify the lazy-load persona router is making
		// reasonable routing decisions without manual session-by-session inspection.
		`CREATE TABLE IF NOT EXISTS prompt_routing_audit (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp       TEXT NOT NULL,
			cli_mode        TEXT NOT NULL DEFAULT '',
			soul_mode       TEXT NOT NULL DEFAULT '',
			signals_json    TEXT NOT NULL DEFAULT '{}',
			fragments_json  TEXT NOT NULL DEFAULT '[]',
			tokens_est      INTEGER NOT NULL DEFAULT 0,
			char_size       INTEGER NOT NULL DEFAULT 0,
			session_id      TEXT NOT NULL DEFAULT '',
			launch_dir      TEXT NOT NULL DEFAULT '',
			source          TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_proute_ts ON prompt_routing_audit(timestamp)`,
		`CREATE INDEX IF NOT EXISTS idx_proute_soul ON prompt_routing_audit(soul_mode)`,
		`CREATE INDEX IF NOT EXISTS idx_proute_cli ON prompt_routing_audit(cli_mode)`,
		// webhook_audit: every POST /webhook invocation. Records action routing,
		// plugin execution result, and any AI fallback session spawned. Used by
		// `weiran webhook tail` for live debugging and by future analytics to
		// detect plugin regressions (rising fallback ratio per action).
		//
		// Schema is deliberately framework-only: no domain-specific columns
		// (URL host, output path, MIME type, etc.) — those belong in plugin
		// internals, not in the framework's audit log.
		`CREATE TABLE IF NOT EXISTS webhook_audit (
			id                  INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp           TEXT NOT NULL,
			request_id          TEXT NOT NULL DEFAULT '',
			action              TEXT NOT NULL DEFAULT '',
			plugin_path         TEXT NOT NULL DEFAULT '',
			exit_code           INTEGER NOT NULL DEFAULT 0,
			duration_ms         INTEGER NOT NULL DEFAULT 0,
			fallback_session_id TEXT NOT NULL DEFAULT '',
			body_json           TEXT NOT NULL DEFAULT '',
			stdout              TEXT NOT NULL DEFAULT '',
			stderr_tail         TEXT NOT NULL DEFAULT '',
			timed_out           BOOLEAN NOT NULL DEFAULT 0,
			status              TEXT NOT NULL DEFAULT '',
			remote_addr         TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_whook_ts ON webhook_audit(timestamp)`,
		`CREATE INDEX IF NOT EXISTS idx_whook_action ON webhook_audit(action)`,
		`CREATE INDEX IF NOT EXISTS idx_whook_status ON webhook_audit(status)`,
		`CREATE INDEX IF NOT EXISTS idx_whook_req ON webhook_audit(request_id)`,
		// content_hook_pending: cross-process queue for PostAssistantMsg.
		// Server writes pending injections when assistant text matches rules;
		// tool-hook binary drains them on next PreToolUse/PostToolUse call.
		`CREATE TABLE IF NOT EXISTS content_hook_pending (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			rule_id    TEXT NOT NULL,
			injection  TEXT NOT NULL,
			created_at TEXT NOT NULL,
			consumed   INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_chp_session ON content_hook_pending(session_id, consumed)`,
	}
	for _, s := range schemas {
		if _, err := db.Exec(s); err != nil {
			db.Close()
			return nil, fmt.Errorf("schema creation failed: %w", err)
		}
	}
	// Idempotent migration for pre-existing DBs that predate the event_name column.
	// SQLite returns an error if the column already exists; we ignore it. The
	// event_name index must be created *after* the ALTER so it works on legacy DBs.
	_, _ = db.Exec(`ALTER TABLE tool_hook_audit ADD COLUMN event_name TEXT NOT NULL DEFAULT 'PreToolUse'`)
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_thook_event ON tool_hook_audit(event_name)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("event_name index creation failed: %w", err)
	}
	// Drop the legacy webhook_audit.host column. Earlier framework drafts
	// stored a host string here, but that's domain-specific data — host
	// classification belongs in the plugin, not the audit log. SQLite 3.35+
	// supports DROP COLUMN; the call errors on DBs that no longer have the
	// column (or never had it), so we ignore the error.
	_, _ = db.Exec(`ALTER TABLE webhook_audit DROP COLUMN host`)
	// soul_sessions table (session lifecycle management)
	if err := ensureSoulSessionTable(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("soul_sessions schema failed: %w", err)
	}
	// FTS5 full-text search over daily notes and session summaries
	if err := ensureFTSSchemas(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("fts schema failed: %w", err)
	}
	// evolve_cycles table: structured log of self-evolution cycles.
	if err := ensureEvolveSchemas(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("evolve schema failed: %w", err)
	}
	return db, nil
}

// recordSessionAgent stores the agent identity for a Claude session ID.
func recordSessionAgent(claudeSID, agentID, agentName, source string) {
	db, err := openDB()
	if err != nil {
		return
	}
	defer db.Close()
	db.Exec(`INSERT OR REPLACE INTO session_agents (claude_session_id, agent_id, agent_name, source, created_at) VALUES (?, ?, ?, ?, ?)`,
		claudeSID, agentID, agentName, source, time.Now().Format(time.RFC3339))
}

// getSessionAgent returns the agent ID for a Claude session ID, or "" if unknown.
func getSessionAgent(claudeSID string) string {
	db, err := openDB()
	if err != nil {
		return ""
	}
	defer db.Close()
	var agent string
	db.QueryRow(`SELECT agent_id FROM session_agents WHERE claude_session_id=?`, claudeSID).Scan(&agent)
	return agent
}

// loadSpawnAgentMap returns a map of session_name → agent_id from the spawns table.
func loadSpawnAgentMap() map[string]string {
	db, err := openDB()
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT session, agent FROM spawns`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	m := make(map[string]string)
	for rows.Next() {
		var session, agent string
		rows.Scan(&session, &agent)
		m[session] = agent
	}
	return m
}

func fileHash(path string) (string, int64) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", 0
	}
	size := info.Size()

	h := sha256.New()
	// large files: hash only first+last 64KB
	if size > 128*1024 {
		buf := make([]byte, 64*1024)
		n, err := f.Read(buf)
		if err != nil {
			return "", 0
		}
		h.Write(buf[:n])
		if _, err := f.Seek(-64*1024, io.SeekEnd); err != nil {
			return "", 0
		}
		n, _ = f.Read(buf)
		h.Write(buf[:n])
		// mix in size as well
		fmt.Fprintf(h, "%d", size)
	} else {
		io.Copy(h, f)
	}
	return hex.EncodeToString(h.Sum(nil)), size
}

type sessionState struct {
	path    string
	size    int64
	hash    string
	changed bool // true = new file or content changed
	summary string
}

func checkSessions(db *sql.DB, files []sessionFile) []sessionState {
	var states []sessionState
	for _, sf := range files {
		hash, size := fileHash(sf.path)
		if hash == "" {
			continue
		}

		st := sessionState{path: sf.path, size: size, hash: hash, changed: true}

		var dbHash string
		var dbSize int64
		var dbSummary string
		err := db.QueryRow("SELECT hash, size, summary FROM sessions WHERE path = ?", sf.path).
			Scan(&dbHash, &dbSize, &dbSummary)
		if err == nil && dbHash == hash && dbSize == size {
			st.changed = false
			st.summary = dbSummary
		}

		states = append(states, st)
	}
	return states
}

// saveSummary saves a single file's summary to DB.
// Also populates summary_seg with Chinese-segmented text for FTS5 indexing.
func saveSummary(db *sql.DB, path, hash string, size int64, summary string) error {
	now := time.Now().Format(time.RFC3339)
	summarySeg := segmentText(summary)
	_, err := db.Exec(`INSERT INTO sessions (path, size, hash, summary, summary_seg, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET size=excluded.size, hash=excluded.hash, summary=excluded.summary, summary_seg=excluded.summary_seg, updated_at=excluded.updated_at`,
		path, size, hash, summary, summarySeg, now)
	return err
}

// ── Session source management ──

// sessionSourcesFile is the path to the JSON file listing archive directories.
// Archives are old .claude backups (e.g. ~/.claude.bak1) whose sessions should
// still be searchable. Each archive is treated like claudeConfigDir — the code
// auto-discovers the projects/ subdirectory within it.
var sessionSourcesFile string // initialized in initSessionSources()

// loadSessionSources reads the list of archive directories.
// Returns nil (not error) if the file doesn't exist.
func loadSessionSources() []string {
	data, err := os.ReadFile(sessionSourcesFile)
	if err != nil {
		return nil
	}
	var dirs []string
	if json.Unmarshal(data, &dirs) != nil {
		return nil
	}
	// Expand ~ and filter to existing directories
	var valid []string
	for _, d := range dirs {
		d = expandHomeStr(d)
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			valid = append(valid, d)
		}
	}
	return valid
}

// saveSessionSources writes the archive directory list.
func saveSessionSources(dirs []string) error {
	// Normalize paths — store with ~ prefix when under home
	var normalized []string
	homePrefix := home + "/"
	for _, d := range dirs {
		d = expandHomeStr(d)
		if strings.HasPrefix(d, homePrefix) {
			d = "~" + d[len(home):]
		}
		normalized = append(normalized, d)
	}
	data, _ := json.MarshalIndent(normalized, "", "  ")
	return os.WriteFile(sessionSourcesFile, data, 0644)
}

// archiveProjectsDirs returns the list of <archive>/projects/ directories
// that actually exist on disk, across all registered archive sources.
func archiveProjectsDirs() []string {
	archives := loadSessionSources()
	var dirs []string
	for _, a := range archives {
		pDir := filepath.Join(a, "projects")
		if fi, err := os.Stat(pDir); err == nil && fi.IsDir() {
			dirs = append(dirs, pDir)
		}
	}
	return dirs
}

// isArchivePath checks whether a file path falls under any registered archive directory.
func isArchivePath(path string) bool {
	for _, a := range loadSessionSources() {
		if strings.HasPrefix(path, a+"/") {
			return true
		}
	}
	return false
}

// expandHomeStr replaces ~ with the user's home directory (swallowing errors).
func expandHomeStr(path string) string {
	p, err := expandHome(path)
	if err != nil {
		return path
	}
	return p
}

// initSessionSources sets the session sources file path.
// Called from initWorkspace or early startup.
func initSessionSources() {
	sessionSourcesFile = filepath.Join(appDir, "session-sources.json")
}

// ── DB subcommands ──

func handleDB(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: " + appName + " db <save|list|stats>")
		return
	}
	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	switch args[0] {
	case "save":
		// weiran db save '{"path":"...","summary":"..."}'
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: "+appName+" db save '<json>'")
			os.Exit(1)
		}
		var input struct {
			Path    string `json:"path"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal([]byte(args[1]), &input); err != nil {
			fmt.Fprintf(os.Stderr, "JSON parse error: %v\n", err)
			os.Exit(1)
		}
		hash, size := fileHash(input.Path)
		if hash == "" {
			fmt.Fprintf(os.Stderr, "file not readable: %s\n", input.Path)
			os.Exit(1)
		}
		if err := saveSummary(db, input.Path, hash, size, input.Summary); err != nil {
			fmt.Fprintf(os.Stderr, "save failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("saved: %s (%d bytes)\n", input.Path, size)

	case "save-batch":
		// ` + appName + ` db save-batch '<json array>'
		// save multiple at once: [{"path":"...","summary":"..."},...]
		// also supports reading from stdin (when arg is - or absent)
		var raw []byte
		if len(args) < 2 || args[1] == "-" {
			raw, _ = io.ReadAll(os.Stdin)
		} else {
			raw = []byte(args[1])
		}
		var inputs []struct {
			Path    string `json:"path"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(raw, &inputs); err != nil {
			fmt.Fprintf(os.Stderr, "JSON parse error: %v\n", err)
			os.Exit(1)
		}
		saved := 0
		for _, input := range inputs {
			hash, size := fileHash(input.Path)
			if hash == "" {
				fmt.Fprintf(os.Stderr, "skipping unreadable: %s\n", input.Path)
				continue
			}
			if err := saveSummary(db, input.Path, hash, size, input.Summary); err != nil {
				fmt.Fprintf(os.Stderr, "save failed for %s: %v\n", input.Path, err)
				continue
			}
			fmt.Printf("saved: %s (%d bytes)\n", input.Path, size)
			saved++
		}
		fmt.Printf("batch save complete: %d/%d\n", saved, len(inputs))

	case "list":
		rows, err := db.Query("SELECT path, size, hash, summary, updated_at FROM sessions ORDER BY updated_at DESC LIMIT 20")
		if err != nil {
			fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
			os.Exit(1)
		}
		defer rows.Close()
		for rows.Next() {
			var path, hash, summary, updatedAt string
			var size int64
			rows.Scan(&path, &size, &hash, &summary, &updatedAt)
			sumPreview := summary
			if len(sumPreview) > 80 {
				sumPreview = sumPreview[:80] + "..."
			}
			fmt.Printf("[%s] %s (%d bytes) hash=%s…\n  %s\n", updatedAt, path, size, hash[:12], sumPreview)
		}

	case "stats":
		var total, withSummary int
		db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&total)
		db.QueryRow("SELECT COUNT(*) FROM sessions WHERE summary != ''").Scan(&withSummary)
		fmt.Printf("total: %d sessions, %d with summary\n", total, withSummary)

	case "recall":
		// weiran db recall — output recall instructions (for claude), on demand
		sessions := recentSessions(20)
		states := checkSessions(db, sessions)
		var pending, done []sessionState
		for _, s := range states {
			if s.changed || s.summary == "" {
				pending = append(pending, s)
			} else {
				done = append(done, s)
			}
		}
		if len(pending) == 0 {
			fmt.Println("all recent sessions already have summaries, no scanning needed.")
			if len(done) > 0 {
				fmt.Println("\nwith summary:")
				for _, s := range done {
					fmt.Printf("  %s → %s\n", filepath.Base(s.path), s.summary)
				}
			}
			return
		}
		fmt.Printf("%d sessions to scan (%d already done).\n\n", len(pending), len(done))
		fmt.Println("files to scan:")
		for _, s := range pending {
			fmt.Printf("  %s (%d bytes)\n", s.path, s.size)
		}
		fmt.Println("\nscanning method:")
		fmt.Println("  - tail last 200-500 lines of each file")
		fmt.Println("  - OpenClaw JSONL：type \"message\", message.role \"user\"")
		fmt.Println("  - Claude Code JSONL：type \"user\", message.content")
		fmt.Println("  - focus on: owner's instructions, decisions, events, emotional exchanges")
		fmt.Println("\nsave after scanning:")
		fmt.Println("  ` + appName + ` db save-batch '[{\"path\":\"...\",\"summary\":\"...\"},...]'")

	case "pending":
		// weiran db pending — output sessions needing scan (JSON format, machine-readable)
		sessions := recentSessions(20)
		states := checkSessions(db, sessions)
		type pendingItem struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
			Hash string `json:"hash"`
		}
		var pending []pendingItem
		for _, s := range states {
			if s.changed || s.summary == "" {
				pending = append(pending, pendingItem{Path: s.path, Size: s.size, Hash: s.hash})
			}
		}
		out, _ := json.MarshalIndent(pending, "", "  ")
		fmt.Println(string(out))

	case "summarized":
		// weiran db summarized — output sessions with existing summaries (JSON)
		sessions := recentSessions(20)
		states := checkSessions(db, sessions)
		type sumItem struct {
			Path    string `json:"path"`
			Summary string `json:"summary"`
		}
		var items []sumItem
		for _, s := range states {
			if !s.changed && s.summary != "" {
				items = append(items, sumItem{Path: s.path, Summary: s.summary})
			}
		}
		out, _ := json.MarshalIndent(items, "", "  ")
		fmt.Println(string(out))

	case "gc":
		// weiran db gc — clean up DB records for sessions no longer on disk.
		// If the session file has been moved into a registered archive (same
		// basename under <archive>/projects/**), relocate the DB path instead
		// of deleting the record.
		rows, err := db.Query("SELECT path FROM sessions")
		if err != nil {
			fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
			os.Exit(1)
		}
		var stale []string
		relocated := map[string]string{} // old → new
		archives := archiveProjectsDirs()
		for rows.Next() {
			var path string
			rows.Scan(&path)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				base := filepath.Base(path)
				found := ""
				for _, archDir := range archives {
					if entries, err := os.ReadDir(archDir); err == nil {
						for _, e := range entries {
							if !e.IsDir() {
								continue
							}
							cand := filepath.Join(archDir, e.Name(), base)
							if _, err := os.Stat(cand); err == nil {
								found = cand
								break
							}
						}
					}
					if found != "" {
						break
					}
				}
				if found != "" {
					relocated[path] = found
				} else {
					stale = append(stale, path)
				}
			}
		}
		rows.Close()
		for oldPath, newPath := range relocated {
			if _, err := db.Exec("UPDATE sessions SET path = ? WHERE path = ?", newPath, oldPath); err != nil {
				fmt.Fprintf(os.Stderr, "  relocate failed %s: %v\n", filepath.Base(oldPath), err)
				continue
			}
			db.Exec("UPDATE session_content SET path = ? WHERE path = ?", newPath, oldPath)
			fmt.Printf("  relocated: %s → archive\n", filepath.Base(oldPath))
		}
		if len(stale) == 0 {
			if len(relocated) > 0 {
				fmt.Printf("relocated %d records to archive, no stale records\n", len(relocated))
			} else {
				fmt.Println("nothing to clean, all DB records have matching files")
			}
			return
		}
		for _, p := range stale {
			db.Exec("DELETE FROM sessions WHERE path = ?", p)
			fmt.Printf("  removed: %s\n", filepath.Base(p))
		}
		fmt.Printf("cleanup done: removed %d stale records", len(stale))
		if len(relocated) > 0 {
			fmt.Printf(", relocated %d to archive", len(relocated))
		}
		fmt.Println()

	case "search":
		// weiran db search <keyword> — search session summaries
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: "+appName+" db search <keyword>")
			os.Exit(1)
		}
		keyword := strings.Join(args[1:], " ")
		rows, err := db.Query("SELECT path, summary, updated_at FROM sessions WHERE summary LIKE ? ORDER BY updated_at DESC LIMIT 20",
			"%"+keyword+"%")
		if err != nil {
			fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
			os.Exit(1)
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var path, summary, updatedAt string
			rows.Scan(&path, &summary, &updatedAt)
			fmt.Printf("\n[%s] %s\n", updatedAt[:10], filepath.Base(path))
			// highlight matching lines
			for _, line := range strings.Split(summary, "\n") {
				if strings.Contains(strings.ToLower(line), strings.ToLower(keyword)) {
					fmt.Printf("  → %s\n", strings.TrimSpace(line))
				}
			}
			count++
		}
		if count == 0 {
			fmt.Printf("no matches containing \"%s\"\n", keyword)
		} else {
			fmt.Printf("\n%d matches\n", count)
		}

	case "patterns":
		jsonMode := len(args) > 1 && args[1] == "-j"
		handlePatterns(db, jsonMode)

	case "pattern-save":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: "+appName+" db pattern-save '{\"name\":\"...\",\"description\":\"...\",\"example\":\"...\",\"source\":\"...\"}'")
			os.Exit(1)
		}
		var input patternInput
		if err := json.Unmarshal([]byte(args[1]), &input); err != nil {
			fmt.Fprintf(os.Stderr, "JSON parse error: %v\n", err)
			os.Exit(1)
		}
		upsertPattern(db, input)

	case "pattern-save-batch":
		var raw []byte
		if len(args) < 2 || args[1] == "-" {
			raw, _ = io.ReadAll(os.Stdin)
		} else {
			raw = []byte(args[1])
		}
		var inputs []patternInput
		if err := json.Unmarshal(raw, &inputs); err != nil {
			fmt.Fprintf(os.Stderr, "JSON parse error: %v\n", err)
			os.Exit(1)
		}
		for _, input := range inputs {
			upsertPattern(db, input)
		}
		fmt.Printf("batch save complete: %d items\n", len(inputs))

	case "feedback":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: "+appName+" db feedback '{\"pattern\":\"...\",\"outcome\":\"success|failure|correction\",\"note\":\"...\",\"session\":\"...\"}'")
			os.Exit(1)
		}
		var input struct {
			Pattern string `json:"pattern"`
			Outcome string `json:"outcome"`
			Note    string `json:"note"`
			Session string `json:"session"`
		}
		if err := json.Unmarshal([]byte(args[1]), &input); err != nil {
			fmt.Fprintf(os.Stderr, "JSON parse error: %v\n", err)
			os.Exit(1)
		}
		if input.Pattern == "" || input.Outcome == "" {
			fmt.Fprintln(os.Stderr, "pattern and outcome required")
			os.Exit(1)
		}
		var pid int
		err := db.QueryRow("SELECT id FROM patterns WHERE name = ?", input.Pattern).Scan(&pid)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pattern not found: %s\n", input.Pattern)
			os.Exit(1)
		}
		now := time.Now().Format(time.RFC3339)
		db.Exec("INSERT INTO pattern_feedback (pattern_id, outcome, note, session, created_at) VALUES (?, ?, ?, ?, ?)",
			pid, input.Outcome, input.Note, input.Session, now)
		fmt.Printf("recorded: %s → %s\n", input.Pattern, input.Outcome)

	case "cultivate":
		dryRun := len(args) > 1 && args[1] == "--dry-run"
		handleCultivate(db, dryRun)

	case "pattern-reject":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: "+appName+" db pattern-reject <name>")
			os.Exit(1)
		}
		res, err := db.Exec("UPDATE patterns SET status = 'rejected' WHERE name = ? AND status = 'candidate'", args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "update failed: %v\n", err)
			os.Exit(1)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			fmt.Fprintf(os.Stderr, "no candidate pattern found: %s\n", args[1])
			os.Exit(1)
		}
		fmt.Printf("rejected: %s\n", args[1])

	case "fts-index":
		// weiran db fts-index — incrementally index daily notes + session content into FTS5
		handleFTSIndex()

	case "fts-index-sessions":
		// weiran db fts-index-sessions — index session JSONL content only
		start := time.Now()
		added, skipped, err := indexSessionContent()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] fts-index-sessions: %v\n", appName, err)
			os.Exit(1)
		}
		fmt.Printf("fts-index-sessions: %d added/updated, %d skipped (%.2fs)\n", added, skipped, time.Since(start).Seconds())

	case "fts-rebuild":
		// weiran db fts-rebuild — drop and recreate FTS5 indexes from source tables
		handleFTSRebuild()

	case "search-fts":
		// weiran db search-fts <query> [--scope=daily|session|content|both] [--limit=N] [--json]
		handleFTSSearch(args[1:])

	case "evolve-log":
		// weiran db evolve-log '<json>' | -
		// Insert one evolve cycle (JSON matches tasks.go wrap-up Summary).
		handleEvolveLog(db, args[1:])

	case "evolve-list":
		// weiran db evolve-list [N]
		// Show the last N cycles (default 10).
		handleEvolveList(db, args[1:])

	case "evolve-backfill":
		// weiran db evolve-backfill [--days N] [--dry-run]
		// Parse past daily notes and import any evolve Summary JSON blocks.
		handleEvolveBackfill(db, args[1:])

	case "events":
		// weiran db events [--since=24h] [--mode=cron] [--type=timeout] [--notify]
		handleEventLog(args[1:])

	case "add-source":
		// weiran db add-source <dir> — register an archive directory
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: "+appName+" db add-source <dir>")
			fmt.Fprintln(os.Stderr, "  Registers a .claude backup directory for session search.")
			fmt.Fprintln(os.Stderr, "  Auto-discovers projects/ subdirectory within it.")
			os.Exit(1)
		}
		dir := expandHomeStr(args[1])
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			fmt.Fprintf(os.Stderr, "directory not found: %s\n", dir)
			os.Exit(1)
		}
		dirs := loadSessionSources()
		abs, _ := filepath.Abs(dir)
		for _, d := range dirs {
			if d == abs {
				fmt.Printf("already registered: %s\n", abs)
				return
			}
		}
		dirs = append(dirs, abs)
		if err := saveSessionSources(dirs); err != nil {
			fmt.Fprintf(os.Stderr, "save failed: %v\n", err)
			os.Exit(1)
		}
		// Check if projects/ subdir exists and count sessions
		projDir := filepath.Join(abs, "projects")
		sessionCount := 0
		if fi, err := os.Stat(projDir); err == nil && fi.IsDir() {
			if entries, err := os.ReadDir(projDir); err == nil {
				for _, e := range entries {
					if e.IsDir() {
						if files, err := os.ReadDir(filepath.Join(projDir, e.Name())); err == nil {
							for _, f := range files {
								if !f.IsDir() && strings.HasSuffix(f.Name(), ".jsonl") {
									sessionCount++
								}
							}
						}
					}
				}
			}
		}
		fmt.Printf("added: %s (found %d sessions in projects/)\n", abs, sessionCount)
		fmt.Printf("run `%s db fts-index-sessions` to index archived sessions\n", appName)

	case "remove-source":
		// weiran db remove-source <dir>
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: "+appName+" db remove-source <dir>")
			os.Exit(1)
		}
		target := expandHomeStr(args[1])
		target, _ = filepath.Abs(target)
		dirs := loadSessionSources()
		var filtered []string
		for _, d := range dirs {
			if d != target {
				filtered = append(filtered, d)
			}
		}
		if len(filtered) == len(dirs) {
			fmt.Fprintf(os.Stderr, "not found: %s\n", target)
			os.Exit(1)
		}
		if err := saveSessionSources(filtered); err != nil {
			fmt.Fprintf(os.Stderr, "save failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("removed: %s\n", target)

	case "migrate-session", "migrate-sessions":
		// weiran db migrate-session <src> [--to <dir>] [--dry-run]
		// Scrub API fingerprints (requestId, message.id) and regenerate
		// sessionId + uuid tree, writing to claudeConfigDir (env-aware)
		// or --to <dir>.
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: "+appName+" db migrate-session <src-file-or-dir> [--to <dir>] [--dry-run] [--keep-source]")
			fmt.Fprintln(os.Stderr, "  Scrubs Anthropic requestId/msg_id, regenerates sessionId + UUID tree,")
			fmt.Fprintln(os.Stderr, "  writes to $CLAUDE_CONFIG_DIR (or --to), updates weiran DB paths.")
			fmt.Fprintln(os.Stderr, "  Source files are deleted after successful migration (use --keep-source to preserve).")
			os.Exit(1)
		}
		srcArg := expandHomeStr(args[1])
		destDir := ""
		dryRun := false
		keepSource := false
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--to":
				if i+1 < len(args) {
					destDir = expandHomeStr(args[i+1])
					i++
				}
			case "--dry-run":
				dryRun = true
			case "--keep-source":
				keepSource = true
			default:
				if strings.HasPrefix(args[i], "--to=") {
					destDir = expandHomeStr(strings.TrimPrefix(args[i], "--to="))
				}
			}
		}
		migrated, err := migrateSessions(srcArg, destDir, dryRun, keepSource)
		if err != nil {
			fmt.Fprintf(os.Stderr, "migrate failed: %v\n", err)
			os.Exit(1)
		}
		// Auto-trigger fts-index-sessions so the just-migrated files land in
		// the content-search index immediately — otherwise the user has to
		// remember a second command. Skip on dry-run (nothing changed on disk)
		// and when nothing actually moved (no DB/index work to do).
		if !dryRun && migrated > 0 {
			start := time.Now()
			added, skipped, ierr := indexSessionContent()
			if ierr != nil {
				fmt.Fprintf(os.Stderr, "  warn: auto fts-index-sessions failed: %v (run `%s db fts-index-sessions` manually)\n", ierr, appName)
			} else {
				fmt.Printf("fts-index-sessions (auto): %d added/updated, %d skipped (%.2fs)\n", added, skipped, time.Since(start).Seconds())
			}
		}

	case "list-sources":
		// weiran db list-sources — show registered archives
		dirs := loadSessionSources()
		if len(dirs) == 0 {
			fmt.Println("no archive sources registered")
			return
		}
		fmt.Println("archive sources:")
		for _, d := range dirs {
			projDir := filepath.Join(d, "projects")
			count := 0
			if entries, err := os.ReadDir(projDir); err == nil {
				for _, e := range entries {
					if e.IsDir() {
						if files, err := os.ReadDir(filepath.Join(projDir, e.Name())); err == nil {
							for _, f := range files {
								if !f.IsDir() && strings.HasSuffix(f.Name(), ".jsonl") {
									count++
								}
							}
						}
					}
				}
			}
			fmt.Printf("  %s (%d sessions)\n", d, count)
		}

	default:
		fmt.Printf("unknown subcommand: %s\nusage: %s db <recall|pending|summarized|save|save-batch|list|stats|search|search-fts|fts-index|fts-index-sessions|fts-rebuild|gc|add-source|remove-source|list-sources|migrate-session|patterns|pattern-save|pattern-save-batch|feedback|cultivate|pattern-reject|events|evolve-log|evolve-list|evolve-backfill>\n", args[0], appName)
	}
}

// importSummaries reads summaries.json written by claude and imports into DB
func importSummaries() {
	data, err := os.ReadFile(sessionTmp("summaries.json"))
	if err != nil {
		fmt.Fprint(os.Stderr, "["+appName+"] no summaries file, skipping import\n")
		return
	}

	var items []struct {
		Path    string `json:"path"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(data, &items); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] summaries JSON parse error: %v\n", err)
		return
	}

	if len(items) == 0 {
		return
	}

	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] %v\n", err)
		return
	}
	defer db.Close()

	saved := 0
	for _, item := range items {
		hash, size := fileHash(item.Path)
		if hash == "" {
			continue
		}
		if err := saveSummary(db, item.Path, hash, size, item.Summary); err != nil {
			fmt.Fprintf(os.Stderr, "["+appName+"] save failed for %s: %v\n", item.Path, err)
			continue
		}
		saved++
	}
	fmt.Fprintf(os.Stderr, "["+appName+"] imported %d/%d summaries\n", saved, len(items))
}

// ── Pattern Cultivation ──

// quality gate thresholds
const (
	cultivateMinSeen      = 5
	cultivateMinDiversity = 2
	cultivateMinReliab    = 0.80
	cultivateMaxNegRate   = 0.25
)

type patternInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Example     string `json:"example"`
	Source      string `json:"source"`
}

type patternRow struct {
	ID          int
	Name        string
	Description string
	Example     string
	Sources     []string
	FirstSeen   string
	LastSeen    string
	SeenCount   int
	Status      string
}

type patternMetrics struct {
	SuccessCount    int
	FailureCount    int
	CorrectionCount int
	TotalFeedback   int
	Reliability     float64 // success / (success+failure)
	NegativeRate    float64 // (failure+correction) / total
	Diversity       int     // len(sources)
}

func upsertPattern(db *sql.DB, input patternInput) {
	now := time.Now().Format(time.RFC3339)

	var existing struct {
		id      int
		sources string
		count   int
	}
	err := db.QueryRow("SELECT id, sources, seen_count FROM patterns WHERE name = ?", input.Name).
		Scan(&existing.id, &existing.sources, &existing.count)

	if err != nil {
		// new pattern
		sources, _ := json.Marshal([]string{input.Source})
		db.Exec(`INSERT INTO patterns (name, description, example, sources, first_seen, last_seen, seen_count, status)
			VALUES (?, ?, ?, ?, ?, ?, 1, 'candidate')`,
			input.Name, input.Description, input.Example, string(sources), now, now)
		fmt.Printf("new pattern: %s\n", input.Name)
		return
	}

	// update existing
	var srcList []string
	if err := json.Unmarshal([]byte(existing.sources), &srcList); err != nil {
		srcList = nil // reset on bad JSON
	}
	if input.Source != "" {
		found := false
		for _, s := range srcList {
			if s == input.Source {
				found = true
				break
			}
		}
		if !found {
			srcList = append(srcList, input.Source)
		}
	}
	srcJSON, _ := json.Marshal(srcList)

	desc := input.Description
	if desc == "" {
		desc = "" // keep DB value (don't overwrite with empty)
		db.Exec(`UPDATE patterns SET sources = ?, last_seen = ?, seen_count = seen_count + 1 WHERE id = ?`,
			string(srcJSON), now, existing.id)
	} else {
		db.Exec(`UPDATE patterns SET description = ?, sources = ?, last_seen = ?, seen_count = seen_count + 1 WHERE id = ?`,
			desc, string(srcJSON), now, existing.id)
	}
	if input.Example != "" {
		db.Exec(`UPDATE patterns SET example = ? WHERE id = ?`, input.Example, existing.id)
	}
	fmt.Printf("updated pattern: %s (seen=%d)\n", input.Name, existing.count+1)
}

func getPatternMetrics(db *sql.DB, patternID int, sources []string) patternMetrics {
	var m patternMetrics
	m.Diversity = len(sources)

	rows, err := db.Query("SELECT outcome, COUNT(*) FROM pattern_feedback WHERE pattern_id = ? GROUP BY outcome", patternID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var outcome string
			var count int
			if rows.Scan(&outcome, &count) == nil {
				switch outcome {
				case "success":
					m.SuccessCount = count
				case "failure":
					m.FailureCount = count
				case "correction":
					m.CorrectionCount = count
				}
			}
		}
	}
	m.TotalFeedback = m.SuccessCount + m.FailureCount + m.CorrectionCount

	if denom := m.SuccessCount + m.FailureCount; denom > 0 {
		m.Reliability = float64(m.SuccessCount) / float64(denom)
	}
	if m.TotalFeedback > 0 {
		m.NegativeRate = float64(m.FailureCount+m.CorrectionCount) / float64(m.TotalFeedback)
	}
	return m
}

func handlePatterns(db *sql.DB, jsonMode bool) {
	rows, err := db.Query("SELECT id, name, description, sources, first_seen, last_seen, seen_count, status FROM patterns WHERE status != 'archived' ORDER BY last_seen DESC")
	if err != nil {
		fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
		return
	}
	defer rows.Close()

	type patternOut struct {
		ID          int            `json:"id"`
		Name        string         `json:"name"`
		Description string         `json:"description"`
		SeenCount   int            `json:"seen_count"`
		Status      string         `json:"status"`
		LastSeen    string         `json:"last_seen"`
		Metrics     patternMetrics `json:"metrics"`
	}
	var items []patternOut

	for rows.Next() {
		var p patternOut
		var sourcesJSON string
		var firstSeen string
		rows.Scan(&p.ID, &p.Name, &p.Description, &sourcesJSON, &firstSeen, &p.LastSeen, &p.SeenCount, &p.Status)
		var sources []string
		json.Unmarshal([]byte(sourcesJSON), &sources)
		p.Metrics = getPatternMetrics(db, p.ID, sources)
		items = append(items, p)
	}

	if jsonMode {
		out, _ := json.MarshalIndent(items, "", "  ")
		fmt.Println(string(out))
		return
	}

	if len(items) == 0 {
		fmt.Println("no patterns yet")
		return
	}
	for _, p := range items {
		gate := "❌"
		if p.SeenCount >= cultivateMinSeen && p.Metrics.Diversity >= cultivateMinDiversity &&
			p.Metrics.Reliability >= cultivateMinReliab && p.Metrics.NegativeRate <= cultivateMaxNegRate &&
			p.Metrics.TotalFeedback > 0 {
			gate = "✅"
		}
		fmt.Printf("[%s] %s  seen=%d div=%d reliab=%.2f neg=%.2f status=%s %s\n",
			p.Status, p.Name, p.SeenCount, p.Metrics.Diversity,
			p.Metrics.Reliability, p.Metrics.NegativeRate, p.Status, gate)
	}
}

func handleCultivate(db *sql.DB, dryRun bool) {
	rows, err := db.Query("SELECT id, name, description, example, sources, seen_count FROM patterns WHERE status = 'candidate'")
	if err != nil {
		fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
		return
	}

	// collect all candidates first, close rows before operating
	type candidate struct {
		id, seenCount       int
		name, desc, example string
		sourcesJSON         string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		rows.Scan(&c.id, &c.name, &c.desc, &c.example, &c.sourcesJSON, &c.seenCount)
		candidates = append(candidates, c)
	}
	rows.Close()

	var promoted int
	for _, c := range candidates {
		var sources []string
		json.Unmarshal([]byte(c.sourcesJSON), &sources)
		m := getPatternMetrics(db, c.id, sources)

		// quality gate
		if c.seenCount < cultivateMinSeen || m.Diversity < cultivateMinDiversity ||
			m.TotalFeedback == 0 || m.Reliability < cultivateMinReliab || m.NegativeRate > cultivateMaxNegRate {
			continue
		}

		if dryRun {
			fmt.Printf("🌱 promotable: %s (seen=%d div=%d reliab=%.2f neg=%.2f)\n",
				c.name, c.seenCount, m.Diversity, m.Reliability, m.NegativeRate)
			continue
		}

		// generate SKILL.md
		skillDir := filepath.Join(appHome, "skills", c.name)
		os.MkdirAll(skillDir, 0755)
		skillPath := filepath.Join(skillDir, "SKILL.md")
		content := generateSkillMD(c.name, c.desc, c.example, sources, c.seenCount, m)
		if err := os.WriteFile(skillPath, []byte(content), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write SKILL.md: %v\n", err)
			continue
		}

		// update status
		db.Exec("UPDATE patterns SET status = 'promoted' WHERE id = ?", c.id)
		promoted++
		fmt.Printf("🎉 promoted: %s → %s\n", c.name, skillPath)
	}

	if dryRun {
		return
	}
	if promoted == 0 {
		fmt.Println("no patterns reached promotion threshold")
	} else {
		fmt.Printf("promoted %d skills this run\n", promoted)
	}
}

func generateSkillMD(name, desc, example string, sources []string, seenCount int, m patternMetrics) string {
	now := time.Now().Format(time.RFC3339)
	sourceList := ""
	for _, s := range sources {
		sourceList += fmt.Sprintf("- %s\n", filepath.Base(s))
	}
	return fmt.Sprintf(`---
name: %s
description: |
  %s
  Auto-cultivated from %d observations.
cultivated: true
cultivated_at: %s
---

# %s

%s

## Example

%s

## Quality

- Seen: %d times
- Source diversity: %d sessions
- Reliability: %.0f%%
- Negative rate: %.0f%%

## Origin

Auto-cultivated from %d session observations.

%s`, name, desc, seenCount, now, name, desc, example, seenCount, m.Diversity,
		m.Reliability*100, m.NegativeRate*100, seenCount, sourceList)
}

// importPatterns reads patterns.json written by claude, batch upsert
func importPatterns() {
	data, err := os.ReadFile(sessionTmp("patterns.json"))
	if err != nil {
		return // no file, normal
	}
	var inputs []patternInput
	if err := json.Unmarshal(data, &inputs); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] patterns JSON parse error: %v\n", err)
		return
	}
	if len(inputs) == 0 {
		return
	}
	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] %v\n", err)
		return
	}
	defer db.Close()
	for _, input := range inputs {
		upsertPattern(db, input)
	}
	fmt.Fprintf(os.Stderr, "["+appName+"] imported %d patterns\n", len(inputs))
}
