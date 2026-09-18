package app

// evolve_db.go — SQLite persistence for the self-evolution cycle log.
//
// The evolve mode already emits a structured JSON summary to its report file
// (see tasks.go wrap-up phase). That JSON is good for the Telegram payload but
// bad for querying — you can't ask "show me the last 3 failed cycles" without
// grepping markdown. This file mirrors the same schema into an SQLite table so
// that:
//
//   1. Future evolve cycles can read prior decisions as structured context
//      (Anti-Pattern Zone, Narrative Memory) instead of re-parsing markdown.
//   2. A SQL explorer UI can expose the cycle history without string parsing.
//   3. Time-series analysis (counter drift, ethics aborts, blast radius trends)
//      becomes SELECT ... GROUP BY instead of custom scripts.
//
// Framework/user-data separation (CLAUDE.md rule): the column structure is
// framework surface (fine to open-source). The row values — specific file paths,
// feedback names, ethics reasons — are the user's private data. We keep the raw
// JSON payload in `raw_json` so schema evolution can backfill without asking
// the user anything.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// evolveCyclesSchema is the DDL for the evolve_cycles table and its indexes.
// Applied idempotently on every openDB() — safe to edit (additions only; use
// ALTER TABLE ADD COLUMN with IF NOT EXISTS semantics for new columns).
const evolveCyclesSchema = `
CREATE TABLE IF NOT EXISTS evolve_cycles (
	id                    INTEGER PRIMARY KEY AUTOINCREMENT,
	cycle_date            TEXT NOT NULL,
	ts                    TEXT NOT NULL DEFAULT (datetime('now')),
	parent_cycle_id       INTEGER,
	status                TEXT NOT NULL DEFAULT 'evolved',

	-- blast radius (flattened for SQL aggregation)
	files_changed         INTEGER NOT NULL DEFAULT 0,
	lines_changed         INTEGER NOT NULL DEFAULT 0,
	skills_touched        INTEGER NOT NULL DEFAULT 0,
	blast_exceeded        INTEGER NOT NULL DEFAULT 0,

	-- invariants
	invariants_checked    INTEGER NOT NULL DEFAULT 0,
	invariants_passed     INTEGER NOT NULL DEFAULT 0,
	invariants_failed     INTEGER NOT NULL DEFAULT 0,
	invariants_violations TEXT NOT NULL DEFAULT '[]',

	-- fact drift
	stale_refs_fixed      INTEGER NOT NULL DEFAULT 0,

	-- feedback
	feedback_new_drafts   INTEGER NOT NULL DEFAULT 0,
	feedback_probed       INTEGER NOT NULL DEFAULT 0,
	feedback_pass         INTEGER NOT NULL DEFAULT 0,
	feedback_fail         INTEGER NOT NULL DEFAULT 0,

	-- skill distill
	sd_archive_candidates INTEGER NOT NULL DEFAULT 0,
	sd_merge_candidates   INTEGER NOT NULL DEFAULT 0,
	sd_new_candidates     INTEGER NOT NULL DEFAULT 0,
	sd_healthy            INTEGER NOT NULL DEFAULT 0,

	-- soul/memory
	soul_memory_files     TEXT NOT NULL DEFAULT '[]',

	-- code evolution (dev builds only; null when absent)
	code_commits          INTEGER NOT NULL DEFAULT 0,
	code_files_changed    INTEGER NOT NULL DEFAULT 0,
	code_tests_passed     INTEGER,
	commit_sha            TEXT NOT NULL DEFAULT '',

	-- ethics gate
	ethics_aborted        INTEGER NOT NULL DEFAULT 0,
	ethics_reasons        TEXT NOT NULL DEFAULT '[]',

	-- review
	needs_human_review    INTEGER NOT NULL DEFAULT 0,

	-- full JSON payload (for schema migration + unknown-field recovery)
	raw_json              TEXT NOT NULL DEFAULT '{}',

	-- source tag: "evolve" (live cycle) | "backfill-daily" (parsed from md)
	source                TEXT NOT NULL DEFAULT 'evolve'
);
`

var evolveCyclesIndexes = []string{
	`CREATE INDEX IF NOT EXISTS idx_evc_date    ON evolve_cycles(cycle_date)`,
	`CREATE INDEX IF NOT EXISTS idx_evc_ts      ON evolve_cycles(ts)`,
	`CREATE INDEX IF NOT EXISTS idx_evc_status  ON evolve_cycles(status)`,
	`CREATE INDEX IF NOT EXISTS idx_evc_review  ON evolve_cycles(needs_human_review)`,
	`CREATE INDEX IF NOT EXISTS idx_evc_parent  ON evolve_cycles(parent_cycle_id)`,
}

// ensureEvolveSchemas creates (or upgrades) the evolve_cycles table and its
// indexes. Idempotent — safe to call on every openDB().
func ensureEvolveSchemas(db *sql.DB) error {
	if _, err := db.Exec(evolveCyclesSchema); err != nil {
		return fmt.Errorf("evolve_cycles create: %w", err)
	}
	for _, idx := range evolveCyclesIndexes {
		if _, err := db.Exec(idx); err != nil {
			return fmt.Errorf("evolve_cycles index: %w", err)
		}
	}
	// Legacy ALTERs for DBs that predate a column. Ignore errors —
	// "duplicate column name" means the migration already ran.
	_, _ = db.Exec(`ALTER TABLE evolve_cycles ADD COLUMN source TEXT NOT NULL DEFAULT 'evolve'`)
	return nil
}

// evolveCyclePayload mirrors the JSON block emitted by tasks.go wrap-up phase.
// Fields are tagged with their JSON keys; missing fields default to zero so
// partial payloads (e.g. no-op cycles) still deserialize cleanly.
type evolveCyclePayload struct {
	CycleDate   string `json:"cycle_date"`
	BlastRadius struct {
		FilesChanged  int  `json:"files_changed"`
		LinesChanged  int  `json:"lines_changed"`
		SkillsTouched int  `json:"skills_touched"`
		Exceeded      bool `json:"exceeded"`
	} `json:"blast_radius"`
	Invariants struct {
		Checked    int      `json:"checked"`
		Passed     int      `json:"passed"`
		Failed     int      `json:"failed"`
		Violations []string `json:"violations"`
	} `json:"invariants"`
	FactDrift struct {
		StaleRefsFixed int `json:"stale_refs_fixed"`
	} `json:"fact_drift"`
	Feedback struct {
		NewDrafts int `json:"new_drafts"`
		Probed    int `json:"probed"`
		Pass      int `json:"pass"`
		Fail      int `json:"fail"`
	} `json:"feedback"`
	SkillDistill struct {
		ArchiveCandidates int `json:"archive_candidates"`
		MergeCandidates   int `json:"merge_candidates"`
		NewCandidates     int `json:"new_candidates"`
		Healthy           int `json:"healthy"`
	} `json:"skill_distill"`
	SoulMemory struct {
		FilesEdited []string `json:"files_edited"`
	} `json:"soul_memory"`
	Code *struct {
		Commits      int  `json:"commits"`
		FilesChanged int  `json:"files_changed"`
		TestsPassed  bool `json:"tests_passed"`
		CommitSha    string `json:"commit_sha"`
	} `json:"code,omitempty"`
	EthicsGate struct {
		Aborted int      `json:"aborted"`
		Reasons []string `json:"reasons"`
	} `json:"ethics_gate"`
	NeedsHumanReview bool   `json:"needs_human_review"`
	Status           string `json:"status"`

	// Local-only metadata (never in the model-generated JSON).
	Source        string `json:"source,omitempty"`
	ParentCycleID int64  `json:"parent_cycle_id,omitempty"`
}

// insertEvolveCycle persists a parsed payload and returns the new row ID.
func insertEvolveCycle(db *sql.DB, p *evolveCyclePayload, rawJSON string) (int64, error) {
	if p.CycleDate == "" {
		p.CycleDate = time.Now().Format("2006-01-02")
	}
	if p.Status == "" {
		p.Status = "evolved"
	}
	if p.Source == "" {
		p.Source = "evolve"
	}
	violationsJSON := mustMarshalJSONArr(p.Invariants.Violations)
	filesJSON := mustMarshalJSONArr(p.SoulMemory.FilesEdited)
	reasonsJSON := mustMarshalJSONArr(p.EthicsGate.Reasons)
	exceeded := boolToInt(p.BlastRadius.Exceeded)
	needsReview := boolToInt(p.NeedsHumanReview)

	var (
		codeCommits    int
		codeFiles      int
		codeTests      sql.NullInt64
		codeCommitSha  string
	)
	if p.Code != nil {
		codeCommits = p.Code.Commits
		codeFiles = p.Code.FilesChanged
		codeTests = sql.NullInt64{Int64: int64(boolToInt(p.Code.TestsPassed)), Valid: true}
		codeCommitSha = p.Code.CommitSha
	}
	var parentVal interface{}
	if p.ParentCycleID > 0 {
		parentVal = p.ParentCycleID
	}

	res, err := db.Exec(`
		INSERT INTO evolve_cycles (
			cycle_date, parent_cycle_id, status,
			files_changed, lines_changed, skills_touched, blast_exceeded,
			invariants_checked, invariants_passed, invariants_failed, invariants_violations,
			stale_refs_fixed,
			feedback_new_drafts, feedback_probed, feedback_pass, feedback_fail,
			sd_archive_candidates, sd_merge_candidates, sd_new_candidates, sd_healthy,
			soul_memory_files,
			code_commits, code_files_changed, code_tests_passed, commit_sha,
			ethics_aborted, ethics_reasons,
			needs_human_review, raw_json, source
		) VALUES (
			?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?,
			?, ?, ?, ?,
			?, ?,
			?, ?, ?
		)`,
		p.CycleDate, parentVal, p.Status,
		p.BlastRadius.FilesChanged, p.BlastRadius.LinesChanged, p.BlastRadius.SkillsTouched, exceeded,
		p.Invariants.Checked, p.Invariants.Passed, p.Invariants.Failed, violationsJSON,
		p.FactDrift.StaleRefsFixed,
		p.Feedback.NewDrafts, p.Feedback.Probed, p.Feedback.Pass, p.Feedback.Fail,
		p.SkillDistill.ArchiveCandidates, p.SkillDistill.MergeCandidates, p.SkillDistill.NewCandidates, p.SkillDistill.Healthy,
		filesJSON,
		codeCommits, codeFiles, codeTests, codeCommitSha,
		p.EthicsGate.Aborted, reasonsJSON,
		needsReview, rawJSON, p.Source,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// handleEvolveLog implements `weiran db evolve-log [<json>|-]`.
// Accepts JSON on stdin (when arg is "-" or omitted) or as a single CLI arg.
// The JSON shape matches the Summary block in tasks.go wrap-up phase.
func handleEvolveLog(db *sql.DB, args []string) {
	var raw []byte
	var err error
	if len(args) == 0 || args[0] == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw = []byte(args[0])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "read failed: %v\n", err)
		os.Exit(1)
	}
	raw = bytesTrimSpace(raw)
	if len(raw) == 0 {
		fmt.Fprintln(os.Stderr, "usage: "+appName+" db evolve-log '<json>' | -")
		os.Exit(1)
	}
	var p evolveCyclePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		fmt.Fprintf(os.Stderr, "JSON parse error: %v\n", err)
		os.Exit(1)
	}
	id, err := insertEvolveCycle(db, &p, string(raw))
	if err != nil {
		fmt.Fprintf(os.Stderr, "insert failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("evolve_cycles: inserted id=%d cycle_date=%s status=%s\n", id, p.CycleDate, p.Status)
}

// handleEvolveList implements `weiran db evolve-list [N]`.
// Prints the most recent N cycles (default 10) in a compact format.
func handleEvolveList(db *sql.DB, args []string) {
	n := 10
	if len(args) > 0 {
		if v, err := parsePositiveInt(args[0]); err == nil {
			n = v
		}
	}
	rows, err := db.Query(`
		SELECT id, cycle_date, ts, status, files_changed, lines_changed,
		       invariants_failed, feedback_new_drafts, ethics_aborted,
		       needs_human_review, source
		FROM evolve_cycles
		ORDER BY ts DESC
		LIMIT ?`, n)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()
	fmt.Printf("%-4s  %-10s  %-19s  %-8s  %5s/%5s  %3s  %3s  %3s  %-14s\n",
		"id", "date", "ts", "status", "files", "lines", "iF", "fB", "eA", "review·src")
	count := 0
	for rows.Next() {
		var (
			id, files, lines, iFailed, fbDrafts, ethAborted, review int
			date, ts, status, source                                string
		)
		if err := rows.Scan(&id, &date, &ts, &status, &files, &lines,
			&iFailed, &fbDrafts, &ethAborted, &review, &source); err != nil {
			fmt.Fprintf(os.Stderr, "scan: %v\n", err)
			continue
		}
		rev := "no"
		if review == 1 {
			rev = "YES"
		}
		fmt.Printf("%-4d  %-10s  %-19s  %-8s  %5d/%5d  %3d  %3d  %3d  %s·%s\n",
			id, date, ts, status, files, lines, iFailed, fbDrafts, ethAborted, rev, source)
		count++
	}
	if count == 0 {
		fmt.Println("(no cycles recorded yet)")
	}
}

// handleEvolveBackfill scans the past daily notes for "Evolve" sections that
// contain a ```json ... ``` block matching the evolve payload schema, and
// inserts any rows not already present (keyed by cycle_date + source).
// Usage: `weiran db evolve-backfill [--days N] [--dry-run]`.
func handleEvolveBackfill(db *sql.DB, args []string) {
	days := 60
	dryRun := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--dry-run":
			dryRun = true
		case args[i] == "--days" && i+1 < len(args):
			if v, err := parsePositiveInt(args[i+1]); err == nil {
				days = v
			}
			i++
		case strings.HasPrefix(args[i], "--days="):
			if v, err := parsePositiveInt(strings.TrimPrefix(args[i], "--days=")); err == nil {
				days = v
			}
		}
	}
	memDir := filepath.Join(workspace, "memory")
	files, err := listDailyNotes(memDir, days)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list daily notes: %v\n", err)
		os.Exit(1)
	}
	existing := loadBackfilledDates(db)
	inserted, skipped, parseErrors := 0, 0, 0
	for _, fp := range files {
		date := dailyNoteDate(fp) // "2026-04-23" or ""
		if date == "" {
			continue
		}
		if existing[date+"|backfill-daily"] {
			skipped++
			continue
		}
		data, err := os.ReadFile(fp)
		if err != nil {
			continue
		}
		payload, rawJSON := extractEvolveJSON(string(data))
		if rawJSON == "" {
			continue
		}
		if payload.CycleDate == "" {
			payload.CycleDate = date
		}
		payload.Source = "backfill-daily"
		if dryRun {
			fmt.Printf("[dry-run] would insert: %s (status=%s)\n", payload.CycleDate, payload.Status)
			inserted++
			continue
		}
		if _, err := insertEvolveCycle(db, &payload, rawJSON); err != nil {
			fmt.Fprintf(os.Stderr, "insert %s: %v\n", date, err)
			parseErrors++
			continue
		}
		inserted++
	}
	fmt.Printf("backfill complete: %d inserted, %d skipped (existing), %d errors\n",
		inserted, skipped, parseErrors)
	if dryRun {
		fmt.Println("(dry-run: no rows actually written)")
	}
}

// ── helpers ──────────────────────────────────────────────────────────

func mustMarshalJSONArr(v []string) string {
	if v == nil {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func parsePositiveInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("not a positive int: %q", s)
	}
	return n, nil
}

// listDailyNotes returns daily notes within the last `days` days, sorted oldest
// first. File name format expected: YYYY-MM-DD.md.
var dailyNoteRE = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.md$`)

func listDailyNotes(dir string, days int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := dailyNoteRE.FindStringSubmatch(e.Name())
		if len(m) != 2 {
			continue
		}
		t, err := time.Parse("2006-01-02", m[1])
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

func dailyNoteDate(fp string) string {
	base := filepath.Base(fp)
	m := dailyNoteRE.FindStringSubmatch(base)
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

// loadBackfilledDates returns a set keyed by "YYYY-MM-DD|source" for rows
// already present, letting backfill be idempotent.
func loadBackfilledDates(db *sql.DB) map[string]bool {
	out := map[string]bool{}
	rows, err := db.Query(`SELECT cycle_date, source FROM evolve_cycles`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var date, src string
		if err := rows.Scan(&date, &src); err == nil {
			out[date+"|"+src] = true
		}
	}
	return out
}

// extractEvolveJSON finds the first fenced ```json ... ``` block in a daily
// note and attempts to decode it as an evolve payload. Returns the parsed
// payload plus the raw JSON text (for storage in raw_json). Empty rawJSON
// means no match / parse failed.
//
// Heuristic: the evolve Summary block has a top-level "cycle_date" field,
// which is distinctive enough to skip unrelated JSON fences (jira reports,
// model configs, etc.).
var jsonFenceRE = regexp.MustCompile("(?s)```json\\s*\\n(\\{.*?\\})\\s*\\n```")

func extractEvolveJSON(content string) (evolveCyclePayload, string) {
	var empty evolveCyclePayload
	matches := jsonFenceRE.FindAllStringSubmatch(content, -1)
	for _, m := range matches {
		if len(m) != 2 {
			continue
		}
		candidate := m[1]
		if !strings.Contains(candidate, `"cycle_date"`) {
			continue
		}
		var p evolveCyclePayload
		if err := json.Unmarshal([]byte(candidate), &p); err != nil {
			continue
		}
		return p, candidate
	}
	return empty, ""
}
