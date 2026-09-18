package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// FTS5 full-text search over daily notes, session summaries, and session content.
//
// Three external-content FTS5 virtual tables are maintained:
//
//   1. daily_notes_fts — indexes markdown files in workspace/memory/*.md
//      (daily diary entries, heartbeat logs, day-by-day history).
//      The daily_notes table is the source of truth; triggers keep the
//      FTS virtual table in sync on insert/update/delete.
//
//   2. session_summaries_fts — references the existing sessions.summary_seg
//      column via external-content mode. No data duplication; triggers on
//      the sessions table keep the index fresh.
//
//   3. session_content_fts — indexes extracted user/assistant text from
//      session JSONL files (both Claude Code and OpenClaw sessions).
//      Enables keyword search over actual conversation content.
//
// Chinese text is pre-segmented with gse (jieba-style) before indexing.
// The *_seg columns store space-separated Chinese words so FTS5's default
// unicode61 tokenizer treats each word as a whole token instead of splitting
// into individual characters.
//
// Entry points:
//   - ensureFTSSchemas(db)     — idempotent schema creation + triggers
//   - indexDailyNotes()        — incremental reindex (mtime+hash skip)
//   - indexSessionContent()    — parse JSONL, extract text, upsert
//   - searchFTS(query, ...)    — unified search, BM25-ranked
//   - fts-index / search-fts / fts-rebuild db subcommands

// ─── schemas ────────────────────────────────────────────────────────────────

const ftsDailyNotesSchema = `
CREATE TABLE IF NOT EXISTS daily_notes (
    date        TEXT PRIMARY KEY,
    path        TEXT NOT NULL,
    content     TEXT NOT NULL,
    content_seg TEXT NOT NULL DEFAULT '',
    mtime       INTEGER NOT NULL,
    hash        TEXT NOT NULL
);`

const ftsDailyNotesVirtual = `
CREATE VIRTUAL TABLE IF NOT EXISTS daily_notes_fts
    USING fts5(date UNINDEXED, content_seg, content=daily_notes, content_rowid=rowid);`

const ftsDailyNotesTriggers = `
CREATE TRIGGER IF NOT EXISTS daily_notes_ai AFTER INSERT ON daily_notes BEGIN
    INSERT INTO daily_notes_fts(rowid, date, content_seg) VALUES (new.rowid, new.date, new.content_seg);
END;

CREATE TRIGGER IF NOT EXISTS daily_notes_au AFTER UPDATE ON daily_notes BEGIN
    INSERT INTO daily_notes_fts(daily_notes_fts, rowid, date, content_seg)
        VALUES('delete', old.rowid, old.date, old.content_seg);
    INSERT INTO daily_notes_fts(rowid, date, content_seg)
        VALUES (new.rowid, new.date, new.content_seg);
END;

CREATE TRIGGER IF NOT EXISTS daily_notes_ad AFTER DELETE ON daily_notes BEGIN
    INSERT INTO daily_notes_fts(daily_notes_fts, rowid, date, content_seg)
        VALUES('delete', old.rowid, old.date, old.content_seg);
END;`

const ftsSessionSummariesVirtual = `
CREATE VIRTUAL TABLE IF NOT EXISTS session_summaries_fts
    USING fts5(path UNINDEXED, summary_seg, content=sessions, content_rowid=rowid);`

const ftsSessionTriggers = `
CREATE TRIGGER IF NOT EXISTS sessions_ai AFTER INSERT ON sessions BEGIN
    INSERT INTO session_summaries_fts(rowid, path, summary_seg) VALUES (new.rowid, new.path, new.summary_seg);
END;

CREATE TRIGGER IF NOT EXISTS sessions_au AFTER UPDATE ON sessions BEGIN
    INSERT INTO session_summaries_fts(session_summaries_fts, rowid, path, summary_seg)
        VALUES('delete', old.rowid, old.path, old.summary_seg);
    INSERT INTO session_summaries_fts(rowid, path, summary_seg)
        VALUES (new.rowid, new.path, new.summary_seg);
END;

CREATE TRIGGER IF NOT EXISTS sessions_ad AFTER DELETE ON sessions BEGIN
    INSERT INTO session_summaries_fts(session_summaries_fts, rowid, path, summary_seg)
        VALUES('delete', old.rowid, old.path, old.summary_seg);
END;`

const sessionContentSchema = `
CREATE TABLE IF NOT EXISTS session_content (
    path     TEXT PRIMARY KEY,
    sid      TEXT NOT NULL,
    text     TEXT NOT NULL,
    text_seg TEXT NOT NULL DEFAULT '',
    mtime    INTEGER NOT NULL,
    hash     TEXT NOT NULL
);`

const sessionContentVirtual = `
CREATE VIRTUAL TABLE IF NOT EXISTS session_content_fts
    USING fts5(sid UNINDEXED, text_seg, content=session_content, content_rowid=rowid);`

const sessionContentTriggers = `
CREATE TRIGGER IF NOT EXISTS session_content_ai AFTER INSERT ON session_content BEGIN
    INSERT INTO session_content_fts(rowid, sid, text_seg) VALUES (new.rowid, new.sid, new.text_seg);
END;

CREATE TRIGGER IF NOT EXISTS session_content_au AFTER UPDATE ON session_content BEGIN
    INSERT INTO session_content_fts(session_content_fts, rowid, sid, text_seg)
        VALUES('delete', old.rowid, old.sid, old.text_seg);
    INSERT INTO session_content_fts(rowid, sid, text_seg)
        VALUES (new.rowid, new.sid, new.text_seg);
END;

CREATE TRIGGER IF NOT EXISTS session_content_ad AFTER DELETE ON session_content BEGIN
    INSERT INTO session_content_fts(session_content_fts, rowid, sid, text_seg)
        VALUES('delete', old.rowid, old.sid, old.text_seg);
END;`

// ensureFTSSchemas is called from openDB alongside ensureSoulSessionTable.
// Idempotent: safe to call on every openDB.
func ensureFTSSchemas(db *sql.DB) error {
	// ── Migration: add *_seg columns if missing ──
	migrateFTSSegColumns(db)

	// daily_notes base table
	if _, err := db.Exec(ftsDailyNotesSchema); err != nil {
		return fmt.Errorf("daily_notes table: %w", err)
	}
	// daily_notes FTS virtual table (external content)
	if _, err := db.Exec(ftsDailyNotesVirtual); err != nil {
		return fmt.Errorf("daily_notes_fts virtual: %w", err)
	}
	// daily_notes triggers
	for _, stmt := range splitSQL(ftsDailyNotesTriggers) {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("daily_notes trigger: %w", err)
		}
	}
	// session_summaries FTS virtual table (external content over sessions)
	if _, err := db.Exec(ftsSessionSummariesVirtual); err != nil {
		return fmt.Errorf("session_summaries_fts virtual: %w", err)
	}
	// sessions triggers
	for _, stmt := range splitSQL(ftsSessionTriggers) {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("sessions trigger: %w", err)
		}
	}
	// First-run rebuild for session_summaries_fts (in case sessions rows exist without index entries)
	// This is safe & cheap even on subsequent calls — the 'rebuild' command reseeds the index.
	var needRebuild int
	db.QueryRow(`SELECT CASE WHEN (SELECT COUNT(*) FROM sessions) > (SELECT COUNT(*) FROM session_summaries_fts) THEN 1 ELSE 0 END`).Scan(&needRebuild)
	if needRebuild == 1 {
		db.Exec(`INSERT INTO session_summaries_fts(session_summaries_fts) VALUES('rebuild')`)
	}
	// session_content base table + FTS + triggers
	if _, err := db.Exec(sessionContentSchema); err != nil {
		return fmt.Errorf("session_content table: %w", err)
	}
	if _, err := db.Exec(sessionContentVirtual); err != nil {
		return fmt.Errorf("session_content_fts virtual: %w", err)
	}
	for _, stmt := range splitSQL(sessionContentTriggers) {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("session_content trigger: %w", err)
		}
	}
	return nil
}

// migrateFTSSegColumns adds *_seg columns and recreates FTS virtual tables + triggers
// for the segmentation upgrade. Safe to call multiple times (checks column existence).
func migrateFTSSegColumns(db *sql.DB) {
	// Check if migration is needed using PRAGMA table_info (robust, no string matching)
	needsMigration := true
	rows, err := db.Query(`PRAGMA table_info(daily_notes)`)
	if err != nil {
		return // table doesn't exist yet, CREATE TABLE will handle it
	}
	defer rows.Close()
	colCount := 0
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dfltValue *string
		var pk int
		if rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk) == nil {
			colCount++
			if name == "content_seg" {
				needsMigration = false
			}
		}
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] migrateFTSSegColumns: iteration error: %v\n", appName, err)
		return
	}
	if colCount == 0 || !needsMigration {
		return // table doesn't exist or already migrated
	}

	fmt.Fprintf(os.Stderr, "[%s] migrating FTS5 to segmented columns...\n", appName)

	// Add *_seg columns to existing tables
	migrations := []struct {
		desc string
		sql  string
	}{
		{"add daily_notes.content_seg", `ALTER TABLE daily_notes ADD COLUMN content_seg TEXT NOT NULL DEFAULT ''`},
		{"add sessions.summary_seg", `ALTER TABLE sessions ADD COLUMN summary_seg TEXT NOT NULL DEFAULT ''`},
		{"add session_content.text_seg", `ALTER TABLE session_content ADD COLUMN text_seg TEXT NOT NULL DEFAULT ''`},
	}
	for _, m := range migrations {
		if _, err := db.Exec(m.sql); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] migration warning (%s): %v\n", appName, m.desc, err)
		}
	}

	// Drop old triggers (they reference old column names) — safe to ignore errors
	for _, name := range []string{
		"daily_notes_ai", "daily_notes_au", "daily_notes_ad",
		"sessions_ai", "sessions_au", "sessions_ad",
		"session_content_ai", "session_content_au", "session_content_ad",
	} {
		db.Exec(`DROP TRIGGER IF EXISTS ` + name)
	}

	// Drop old FTS virtual tables (schema changed)
	for _, tbl := range []string{"daily_notes_fts", "session_summaries_fts", "session_content_fts"} {
		db.Exec(`DROP TABLE IF EXISTS ` + tbl)
	}

	// Backfill *_seg columns from existing content
	backfillSegColumns(db)

	fmt.Fprintf(os.Stderr, "[%s] FTS5 migration complete. Run `%s db fts-rebuild` to rebuild indexes.\n", appName, appName)
}

// backfillSegColumns populates *_seg columns by segmenting existing content.
func backfillSegColumns(db *sql.DB) {
	backfillOne := func(table, srcCol, segCol string) {
		query := fmt.Sprintf(`SELECT rowid, %s FROM %s WHERE %s = ''`, srcCol, table, segCol)
		rows, err := db.Query(query)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] backfill %s: query failed: %v\n", appName, table, err)
			return
		}
		// Read all rows into memory first to avoid SQLite busy errors
		// from concurrent read cursor + write on the same connection.
		type rowData struct {
			rowid   int64
			content string
		}
		var pending []rowData
		for rows.Next() {
			var rd rowData
			if err := rows.Scan(&rd.rowid, &rd.content); err != nil {
				continue
			}
			pending = append(pending, rd)
		}
		if err := rows.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] backfill %s: iteration error: %v\n", appName, table, err)
		}
		rows.Close()

		var updated, failed int
		for _, rd := range pending {
			seg := segmentText(rd.content)
			if _, err := db.Exec(fmt.Sprintf(`UPDATE %s SET %s = ? WHERE rowid = ?`, table, segCol), seg, rd.rowid); err != nil {
				failed++
			} else {
				updated++
			}
		}
		if failed > 0 {
			fmt.Fprintf(os.Stderr, "[%s] backfill %s: %d updated, %d failed\n", appName, table, updated, failed)
		}
	}

	backfillOne("daily_notes", "content", "content_seg")
	backfillOne("sessions", "summary", "summary_seg")
	backfillOne("session_content", "text", "text_seg")
}

// splitSQL breaks a multi-statement SQL string on semicolons (ignoring empty
// statements). SQLite's db.Exec accepts multiple statements in one call for
// most drivers, but modernc.org/sqlite is stricter about trigger bodies, so
// we split at the top-level statement boundary. Triggers themselves use
// BEGIN/END blocks and nested semicolons are kept together.
func splitSQL(s string) []string {
	// Our CREATE TRIGGER blocks terminate with "END;" — split on that
	// sentinel and then clean up.
	var out []string
	parts := strings.SplitAfter(s, "END;")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{strings.TrimSpace(s)}
	}
	return out
}

// ─── daily notes indexing ──────────────────────────────────────────────────

// dailyNoteDatePattern matches filenames like "2026-04-09.md"
var dailyNoteDatePattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.md$`)

// indexDailyNotes walks workspace/memory/daily/*.md and upserts daily notes.
// Heartbeat logs (memory/daily/heartbeat/*.md) are indexed separately with
// a "hb:" date prefix so they don't pollute normal daily note search results.
// Unchanged files (matching mtime+hash) are skipped. Returns (added, skipped, err).
func indexDailyNotes() (int, int, error) {
	db, err := openDB()
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()

	totalAdded, totalSkipped := 0, 0

	// Index normal daily notes
	a, s, err := indexDailyDir(db, filepath.Join(workspace, "memory", "daily"), "")
	if err != nil {
		return 0, 0, err
	}
	totalAdded += a
	totalSkipped += s

	// Index heartbeat daily notes (date key prefixed with "hb:" to avoid collisions)
	a, s, _ = indexDailyDir(db, filepath.Join(workspace, "memory", "daily", "heartbeat"), "hb:")
	totalAdded += a
	totalSkipped += s

	return totalAdded, totalSkipped, nil
}

// indexDailyDir indexes all YYYY-MM-DD.md files in a directory into the daily_notes table.
// datePrefix is prepended to the date key (e.g. "hb:" for heartbeat logs, "" for normal notes).
func indexDailyDir(db *sql.DB, dir, datePrefix string) (int, int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Heartbeat dir may not exist yet — not an error
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("read dir %s: %w", dir, err)
	}

	added, skipped := 0, 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := dailyNoteDatePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		date := datePrefix + m[1]
		path := filepath.Join(dir, e.Name())

		info, err := e.Info()
		if err != nil {
			continue
		}
		mtime := info.ModTime().Unix()
		hash, _ := fileHash(path)
		if hash == "" {
			continue
		}

		// Skip unchanged files
		var existingHash string
		var existingMtime int64
		err = db.QueryRow(`SELECT hash, mtime FROM daily_notes WHERE date = ?`, date).Scan(&existingHash, &existingMtime)
		if err == nil {
			if existingHash == hash && existingMtime == mtime {
				skipped++
				continue
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			fmt.Fprintf(os.Stderr, "[%s] fts query daily_notes %s: %v\n", appName, date, err)
			continue
		}

		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		// Strip YAML frontmatter for cleaner search (keeps body only)
		body := stripFrontmatter(string(content))

		// Segment Chinese text for FTS5
		bodySeg := segmentText(body)

		_, err = db.Exec(
			`INSERT INTO daily_notes(date, path, content, content_seg, mtime, hash) VALUES(?,?,?,?,?,?)
			 ON CONFLICT(date) DO UPDATE SET path=excluded.path, content=excluded.content, content_seg=excluded.content_seg, mtime=excluded.mtime, hash=excluded.hash`,
			date, path, body, bodySeg, mtime, hash,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] fts index %s: %v\n", appName, date, err)
			continue
		}
		added++
	}
	return added, skipped, nil
}

// stripFrontmatter removes YAML frontmatter from markdown content.
// Returns body only. If no frontmatter, returns content unchanged.
func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return s
	}
	// Find the closing ---
	rest := s[4:]
	if i := strings.Index(rest, "\n---\n"); i >= 0 {
		return strings.TrimLeft(rest[i+5:], "\r\n") // skip "\n---\n"
	}
	if i := strings.Index(rest, "\n---\r\n"); i >= 0 {
		return strings.TrimLeft(rest[i+6:], "\r\n") // skip "\n---\r\n"
	}
	return s // malformed, return as-is
}

// rebuildFTS drops and recreates the FTS indexes from their underlying tables.
// Used as an escape hatch when schemas drift or index is corrupted.
func rebuildFTS() error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	// daily_notes_fts rebuild
	if _, err := db.Exec(`INSERT INTO daily_notes_fts(daily_notes_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild daily_notes_fts: %w", err)
	}
	// session_summaries_fts rebuild
	if _, err := db.Exec(`INSERT INTO session_summaries_fts(session_summaries_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild session_summaries_fts: %w", err)
	}
	// session_content_fts rebuild
	if _, err := db.Exec(`INSERT INTO session_content_fts(session_content_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild session_content_fts: %w", err)
	}
	return nil
}

// ─── session content indexing ──────────────────────────────────────────────

// jsonlEvent is a minimal struct for parsing session JSONL lines.
// Handles both OpenClaw format (type:"message", message.role:"user") and
// Claude Code format (type:"user"/"assistant", message.role:"user"/"assistant").
type jsonlEvent struct {
	Type    string `json:"type"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// isTextEvent returns true and the effective role for events containing searchable text.
func (e *jsonlEvent) isTextEvent() (bool, string) {
	switch e.Type {
	case "message":
		// OpenClaw format: type="message", role in message.role
		if e.Message.Role == "user" || e.Message.Role == "assistant" {
			return true, e.Message.Role
		}
	case "user":
		// Claude Code format: type="user", message.role="user"
		return true, "user"
	case "assistant":
		// Claude Code format: type="assistant", message.role="assistant"
		return true, "assistant"
	}
	return false, ""
}

// jsonlTextBlock represents a single text block in a message content array.
type jsonlTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// extractSessionText reads a JSONL file and extracts user/assistant text content.
// Returns a concatenated string with role prefixes for searchable context.
// Returns empty string if no text content found.
func extractSessionText(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var buf strings.Builder
	decoder := json.NewDecoder(f)
	for decoder.More() {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			continue
		}
		var ev jsonlEvent
		if json.Unmarshal(raw, &ev) != nil {
			continue
		}
		ok, role := ev.isTextEvent()
		if !ok {
			continue
		}

		// Content can be a string or an array of blocks
		var texts []string
		// Try as array first
		var blocks []jsonlTextBlock
		if json.Unmarshal(ev.Message.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "text" && b.Text != "" {
					texts = append(texts, b.Text)
				}
			}
		} else {
			// Try as plain string
			var s string
			if json.Unmarshal(ev.Message.Content, &s) == nil && s != "" {
				texts = append(texts, s)
			}
		}

		for _, t := range texts {
			// Truncate individual text blocks to 2KB to keep index manageable
			if len(t) > 2048 {
				t = t[:2048]
			}
			buf.WriteString(role)
			buf.WriteString(": ")
			buf.WriteString(t)
			buf.WriteString("\n")
		}
	}
	return buf.String()
}

// indexSessionContent walks both Claude Code and OpenClaw session directories,
// parses JSONL files, extracts text content, and indexes into session_content.
// Unchanged files (matching hash) are skipped. Returns (added, skipped, err).
func indexSessionContent() (int, int, error) {
	var jsonlFiles []string

	// Claude Code sessions
	ccDir := filepath.Join(claudeConfigDir, "projects")
	if entries, err := os.ReadDir(ccDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				// Could be a .jsonl file directly
				if strings.HasSuffix(e.Name(), ".jsonl") {
					jsonlFiles = append(jsonlFiles, filepath.Join(ccDir, e.Name()))
				}
				continue
			}
			dir := filepath.Join(ccDir, e.Name())
			if subentries, err := os.ReadDir(dir); err == nil {
				for _, se := range subentries {
					if !se.IsDir() && strings.HasSuffix(se.Name(), ".jsonl") {
						jsonlFiles = append(jsonlFiles, filepath.Join(dir, se.Name()))
					}
				}
			}
		}
	}

	// Archive sources — old .claude backups registered via `db add-source`
	for _, archDir := range archiveProjectsDirs() {
		if entries, err := os.ReadDir(archDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					if strings.HasSuffix(e.Name(), ".jsonl") {
						jsonlFiles = append(jsonlFiles, filepath.Join(archDir, e.Name()))
					}
					continue
				}
				dir := filepath.Join(archDir, e.Name())
				if subentries, err := os.ReadDir(dir); err == nil {
					for _, se := range subentries {
						if !se.IsDir() && strings.HasSuffix(se.Name(), ".jsonl") {
							jsonlFiles = append(jsonlFiles, filepath.Join(dir, se.Name()))
						}
					}
				}
			}
		}
	}

	if len(jsonlFiles) == 0 {
		return 0, 0, nil
	}

	db, err := openDB()
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()

	added, skipped := 0, 0
	for _, path := range jsonlFiles {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		mtime := info.ModTime().Unix()
		hash, _ := fileHash(path)
		if hash == "" {
			continue
		}

		// Skip unchanged files
		var existingHash string
		var existingMtime int64
		err = db.QueryRow(`SELECT hash, mtime FROM session_content WHERE path = ?`, path).Scan(&existingHash, &existingMtime)
		if err == nil {
			if existingHash == hash && existingMtime == mtime {
				skipped++
				continue
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			fmt.Fprintf(os.Stderr, "[%s] fts query session_content %s: %v\n", appName, filepath.Base(path), err)
			continue
		}

		text := extractSessionText(path)
		if text == "" {
			skipped++
			continue
		}

		// Segment Chinese text for FTS5
		textSeg := segmentText(text)

		// Extract session ID from filename (UUID without extension)
		sid := strings.TrimSuffix(filepath.Base(path), ".jsonl")

		_, err = db.Exec(
			`INSERT INTO session_content(path, sid, text, text_seg, mtime, hash) VALUES(?,?,?,?,?,?)
			 ON CONFLICT(path) DO UPDATE SET sid=excluded.sid, text=excluded.text, text_seg=excluded.text_seg, mtime=excluded.mtime, hash=excluded.hash`,
			path, sid, text, textSeg, mtime, hash,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] fts session %s: %v\n", appName, filepath.Base(path), err)
			continue
		}
		added++
	}
	return added, skipped, nil
}

// ─── search ────────────────────────────────────────────────────────────────

type ftsHit struct {
	Source  string  `json:"source"`            // "daily" | "session" | "content"
	Date    string  `json:"date,omitempty"`    // daily: "2026-04-09"
	Path    string  `json:"path,omitempty"`    // session: session file path
	Sid     string  `json:"sid,omitempty"`     // content: session ID
	Snippet string  `json:"snippet"`           // FTS5 snippet() with [..] highlight
	Rank    float64 `json:"rank"`              // BM25 score (lower = better match)
}

// searchFTS runs MATCH against one or more FTS5 virtual tables.
// scope: "daily" | "session" | "content" | "heartbeat" | "both" (default = all except heartbeat).
// query: raw user input, will be segmented and sanitized.
// limit: max rows per source.
// sanitizeFTSQuery makes a user query safe for FTS5 MATCH by:
//  1. Segmenting Chinese text with gse (jieba-style)
//  2. Quoting each token as a phrase
//
// This ensures Chinese multi-character words like "巡检" are matched as
// whole words rather than individual characters.
func sanitizeFTSQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	// Segment Chinese text in the query
	q = segmentText(q)
	fields := strings.Fields(q)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		// Escape embedded double-quotes (FTS5: "" inside "...")
		f = strings.ReplaceAll(f, `"`, `""`)
		out = append(out, `"`+f+`"`)
	}
	return strings.Join(out, " ")
}

func searchFTS(query, scope string, limit int) ([]ftsHit, error) {
	if limit <= 0 {
		limit = 20
	}
	if scope == "" || scope == "both" {
		scope = "all" // "both" is legacy alias for searching all sources
	}
	match := sanitizeFTSQuery(query)
	if match == "" {
		return nil, nil
	}
	db, err := openDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var hits []ftsHit

	if scope == "daily" || scope == "heartbeat" || scope == "all" {
		// Build WHERE clause: filter by date prefix for heartbeat vs normal.
		// "all" (default) excludes heartbeat to keep search clean — use "heartbeat" scope explicitly.
		dateFilter := ""
		switch scope {
		case "daily", "all":
			dateFilter = ` AND daily_notes.date NOT LIKE 'hb:%'`
		case "heartbeat":
			dateFilter = ` AND daily_notes.date LIKE 'hb:%'`
		}
		q := fmt.Sprintf(`
			SELECT daily_notes.date, snippet(daily_notes_fts, 1, '[', ']', ' … ', 24), rank
			FROM daily_notes_fts
			JOIN daily_notes ON daily_notes.rowid = daily_notes_fts.rowid
			WHERE daily_notes_fts MATCH ?%s
			ORDER BY rank
			LIMIT ?`, dateFilter)
		rows, err := db.Query(q, match, limit)
		if err != nil {
			return nil, fmt.Errorf("daily search: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var h ftsHit
			if scope == "heartbeat" {
				h.Source = "heartbeat"
			} else {
				h.Source = "daily"
			}
			if err := rows.Scan(&h.Date, &h.Snippet, &h.Rank); err != nil {
				continue
			}
			// Strip "hb:" prefix from date for display
			h.Date = strings.TrimPrefix(h.Date, "hb:")
			hits = append(hits, h)
		}
		if err := rows.Err(); err != nil {
			return hits, fmt.Errorf("daily search iteration: %w", err)
		}
	}

	if scope == "session" || scope == "all" {
		rows, err := db.Query(`
			SELECT path, snippet(session_summaries_fts, 1, '[', ']', ' … ', 24), rank
			FROM session_summaries_fts
			WHERE session_summaries_fts MATCH ?
			ORDER BY rank
			LIMIT ?`, match, limit)
		if err != nil {
			return nil, fmt.Errorf("session search: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var h ftsHit
			h.Source = "session"
			if err := rows.Scan(&h.Path, &h.Snippet, &h.Rank); err != nil {
				continue
			}
			hits = append(hits, h)
		}
		if err := rows.Err(); err != nil {
			return hits, fmt.Errorf("session search iteration: %w", err)
		}
	}

	if scope == "content" || scope == "all" {
		rows, err := db.Query(`
			SELECT sid, snippet(session_content_fts, 1, '[', ']', ' … ', 24), rank
			FROM session_content_fts
			WHERE session_content_fts MATCH ?
			ORDER BY rank
			LIMIT ?`, match, limit)
		if err != nil {
			return nil, fmt.Errorf("content search: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var h ftsHit
			h.Source = "content"
			if err := rows.Scan(&h.Sid, &h.Snippet, &h.Rank); err != nil {
				continue
			}
			hits = append(hits, h)
		}
		if err := rows.Err(); err != nil {
			return hits, fmt.Errorf("content search iteration: %w", err)
		}
	}

	return hits, nil
}

// ─── CLI handlers ──────────────────────────────────────────────────────────

// handleFTSIndex implements `weiran db fts-index`
func handleFTSIndex() {
	start := time.Now()

	// Index daily notes
	dAdded, dSkipped, err := indexDailyNotes()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] fts-index notes: %v\n", appName, err)
		os.Exit(1)
	}

	// Index session content
	sAdded, sSkipped, err := indexSessionContent()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] fts-index sessions: %v\n", appName, err)
		os.Exit(1)
	}

	fmt.Printf("fts-index: notes %d/%d, sessions %d/%d (%.2fs)\n",
		dAdded, dSkipped, sAdded, sSkipped, time.Since(start).Seconds())
}

// handleFTSRebuild implements `weiran db fts-rebuild`
func handleFTSRebuild() {
	fmt.Println("backfilling segmented columns...")
	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] fts-rebuild open: %v\n", appName, err)
		os.Exit(1)
	}
	defer db.Close()
	backfillSegColumns(db)

	if err := rebuildFTS(); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] fts-rebuild: %v\n", appName, err)
		os.Exit(1)
	}
	fmt.Println("fts-rebuild: ok")
}

// handleFTSSearch implements `weiran db search-fts <query> [--scope=daily|session|content|heartbeat|both] [--limit=N] [--json]`
func handleFTSSearch(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: "+appName+" db search-fts <query> [--scope=daily|session|content|heartbeat|both] [--limit=N] [--json]")
		os.Exit(1)
	}
	scope := "both"
	limit := 20
	asJSON := false
	var queryParts []string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--scope="):
			scope = strings.TrimPrefix(a, "--scope=")
		case strings.HasPrefix(a, "--limit="):
			fmt.Sscanf(strings.TrimPrefix(a, "--limit="), "%d", &limit)
		case a == "--json":
			asJSON = true
		default:
			queryParts = append(queryParts, a)
		}
	}
	query := strings.Join(queryParts, " ")
	if query == "" {
		fmt.Fprintln(os.Stderr, "empty query")
		os.Exit(1)
	}

	hits, err := searchFTS(query, scope, limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] search-fts: %v\n", appName, err)
		os.Exit(1)
	}

	if asJSON {
		out, _ := json.MarshalIndent(hits, "", "  ")
		fmt.Println(string(out))
		return
	}

	if len(hits) == 0 {
		fmt.Printf("no matches for %q (scope=%s)\n", query, scope)
		return
	}
	for _, h := range hits {
		switch h.Source {
		case "daily":
			fmt.Printf("\n📅 [%s] rank=%.2f\n   %s\n", h.Date, h.Rank, h.Snippet)
		case "content":
			sidDisplay := h.Sid
			if len(sidDisplay) > 12 {
				sidDisplay = sidDisplay[:12]
			}
			fmt.Printf("\n💬 sid=%s rank=%.2f\n   %s\n", sidDisplay, h.Rank, h.Snippet)
		default:
			fmt.Printf("\n💬 %s rank=%.2f\n   %s\n", filepath.Base(h.Path), h.Rank, h.Snippet)
		}
	}
	fmt.Printf("\n%d hit(s)\n", len(hits))
}
