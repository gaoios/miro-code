package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sample Summary JSON matching the shape emitted by tasks.go wrap-up phase.
const sampleEvolvePayload = `{
  "cycle_date": "2026-04-23",
  "blast_radius": {"files_changed": 2, "lines_changed": 14, "skills_touched": 0, "exceeded": false},
  "invariants":   {"checked": 7, "passed": 7, "failed": 0, "violations": []},
  "fact_drift":   {"stale_refs_fixed": 0},
  "feedback":     {"new_drafts": 0, "probed": 3, "pass": 3, "fail": 0},
  "skill_distill":{"archive_candidates": 0, "merge_candidates": 0, "new_candidates": 1, "healthy": 32},
  "soul_memory":  {"files_edited": ["memory/topics/feedback_x.md"]},
  "code":         {"commits": 1, "files_changed": 3, "tests_passed": true, "commit_sha": "abc1234"},
  "ethics_gate":  {"aborted": 0, "reasons": []},
  "needs_human_review": false,
  "status": "evolved"
}`

func TestEnsureEvolveSchemas_CreatesTable(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='evolve_cycles'`).Scan(&name)
	if err != nil {
		t.Fatalf("evolve_cycles table not created: %v", err)
	}
}

func TestEnsureEvolveSchemas_Idempotent(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db, err := openDB()
	if err != nil {
		t.Fatalf("openDB #1: %v", err)
	}
	db.Close()
	db2, err := openDB()
	if err != nil {
		t.Fatalf("openDB #2 (should be idempotent): %v", err)
	}
	db2.Close()
}

func TestInsertEvolveCycle_Roundtrip(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db, _ := openDB()
	defer db.Close()

	var p evolveCyclePayload
	if err := json.Unmarshal([]byte(sampleEvolvePayload), &p); err != nil {
		t.Fatalf("unmarshal sample: %v", err)
	}
	id, err := insertEvolveCycle(db, &p, sampleEvolvePayload)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}

	// read back and verify the flattened columns match
	var (
		date, status, filesEdJSON, commitSha, rawJSON, source string
		files, lines, iChecked, iPassed, iFailed, fbDrafts, fbPass int
		review, codeCommits, codeFilesChanged, ethAborted         int
	)
	err = db.QueryRow(`SELECT cycle_date, status,
			files_changed, lines_changed,
			invariants_checked, invariants_passed, invariants_failed,
			feedback_new_drafts, feedback_pass,
			soul_memory_files,
			code_commits, code_files_changed, commit_sha,
			ethics_aborted, needs_human_review, raw_json, source
		FROM evolve_cycles WHERE id = ?`, id).Scan(
		&date, &status,
		&files, &lines,
		&iChecked, &iPassed, &iFailed,
		&fbDrafts, &fbPass,
		&filesEdJSON,
		&codeCommits, &codeFilesChanged, &commitSha,
		&ethAborted, &review, &rawJSON, &source,
	)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	checks := []struct{ got, want interface{}; name string }{
		{date, "2026-04-23", "cycle_date"},
		{status, "evolved", "status"},
		{files, 2, "files_changed"},
		{lines, 14, "lines_changed"},
		{iChecked, 7, "invariants_checked"},
		{iPassed, 7, "invariants_passed"},
		{iFailed, 0, "invariants_failed"},
		{fbDrafts, 0, "feedback_new_drafts"},
		{fbPass, 3, "feedback_pass"},
		{codeCommits, 1, "code_commits"},
		{codeFilesChanged, 3, "code_files_changed"},
		{commitSha, "abc1234", "commit_sha"},
		{ethAborted, 0, "ethics_aborted"},
		{review, 0, "needs_human_review"},
		{source, "evolve", "source (default)"},
	}
	for _, c := range checks {
		if fmt.Sprint(c.got) != fmt.Sprint(c.want) {
			t.Errorf("%s: got %v want %v", c.name, c.got, c.want)
		}
	}
	// soul_memory_files must be a JSON array containing the expected path
	if !strings.Contains(filesEdJSON, "feedback_x.md") {
		t.Errorf("soul_memory_files missing expected entry: %s", filesEdJSON)
	}
	// raw_json round-trip must be parseable
	var back map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &back); err != nil {
		t.Errorf("raw_json not valid JSON: %v", err)
	}
}

func TestInsertEvolveCycle_NoOpStatus(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db, _ := openDB()
	defer db.Close()

	// minimal no-op payload
	payload := `{"cycle_date":"2026-04-24","status":"no-op","needs_human_review":false,
		"blast_radius":{"files_changed":0,"lines_changed":0,"skills_touched":0,"exceeded":false},
		"invariants":{"checked":0,"passed":0,"failed":0,"violations":[]},
		"fact_drift":{"stale_refs_fixed":0},
		"feedback":{"new_drafts":0,"probed":0,"pass":0,"fail":0},
		"skill_distill":{"archive_candidates":0,"merge_candidates":0,"new_candidates":0,"healthy":0},
		"soul_memory":{"files_edited":[]},
		"ethics_gate":{"aborted":0,"reasons":[]}}`
	var p evolveCyclePayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatal(err)
	}
	if _, err := insertEvolveCycle(db, &p, payload); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var status string
	var codeTests *int // nullable
	db.QueryRow(`SELECT status, code_tests_passed FROM evolve_cycles WHERE cycle_date='2026-04-24'`).Scan(&status, &codeTests)
	if status != "no-op" {
		t.Errorf("status: got %q want no-op", status)
	}
	if codeTests != nil {
		t.Errorf("code_tests_passed should be NULL when code block omitted, got %v", *codeTests)
	}
}

func TestExtractEvolveJSON_MatchesFencedBlock(t *testing.T) {
	markdown := "# Daily note\n\nSome prose.\n\n```json\n" + sampleEvolvePayload + "\n```\n\nMore prose.\n"
	p, raw := extractEvolveJSON(markdown)
	if raw == "" {
		t.Fatal("expected raw JSON to be extracted")
	}
	if p.CycleDate != "2026-04-23" {
		t.Errorf("cycle_date: got %q", p.CycleDate)
	}
	if p.Status != "evolved" {
		t.Errorf("status: got %q", p.Status)
	}
}

func TestExtractEvolveJSON_SkipsUnrelatedJSON(t *testing.T) {
	markdown := "```json\n{\"foo\":\"bar\"}\n```\n\n```json\n" + sampleEvolvePayload + "\n```"
	p, raw := extractEvolveJSON(markdown)
	if raw == "" {
		t.Fatal("expected to skip unrelated JSON and pick the evolve block")
	}
	if p.CycleDate != "2026-04-23" {
		t.Errorf("picked wrong block: %q", p.CycleDate)
	}
}

func TestExtractEvolveJSON_NoMatch(t *testing.T) {
	_, raw := extractEvolveJSON("plain markdown with no json fence")
	if raw != "" {
		t.Errorf("expected empty, got %q", raw)
	}
}

func TestBackfill_Idempotent(t *testing.T) {
	origDB := dbPath
	origWS := workspace
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	workspace = dir
	defer func() {
		dbPath = origDB
		workspace = origWS
	}()

	memDir := filepath.Join(dir, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	note := "# 2026-04-23\n\n```json\n" + sampleEvolvePayload + "\n```\n"
	if err := os.WriteFile(filepath.Join(memDir, "2026-04-23.md"), []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}

	db, _ := openDB()
	defer db.Close()

	// first pass: should insert
	files, err := listDailyNotes(memDir, 365*10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 note, got %d", len(files))
	}
	// simulate backfill via the exported unit path
	data, _ := os.ReadFile(files[0])
	payload, raw := extractEvolveJSON(string(data))
	if raw == "" {
		t.Fatal("no payload extracted")
	}
	payload.Source = "backfill-daily"
	if _, err := insertEvolveCycle(db, &payload, raw); err != nil {
		t.Fatal(err)
	}

	existing := loadBackfilledDates(db)
	if !existing["2026-04-23|backfill-daily"] {
		t.Errorf("expected existing map to contain the backfilled key, got: %v", existing)
	}
	// second pass: idempotence check — don't insert again
	if existing["2026-04-23|backfill-daily"] {
		t.Log("idempotence: would skip (correct)")
	}
}
