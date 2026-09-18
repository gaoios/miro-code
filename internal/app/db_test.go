package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenDB_CreatesTable(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db, _ := openDB()
	defer db.Close()

	// verify table exists
	var name string
	err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='sessions'").Scan(&name)
	if err != nil {
		t.Fatalf("sessions table not created: %v", err)
	}
}

func TestOpenDB_Idempotent(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db1, _ := openDB()
	db1.Close()
	db2, _ := openDB()
	db2.Close()
	// Should not panic on second open
}

func TestSaveSummary_InsertAndUpdate(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db, _ := openDB()
	defer db.Close()

	// Insert
	saveSummary(db, "/tmp/test.jsonl", "hash1", 100, "first summary")

	var summary string
	db.QueryRow("SELECT summary FROM sessions WHERE path = ?", "/tmp/test.jsonl").Scan(&summary)
	if summary != "first summary" {
		t.Errorf("summary = %q, want 'first summary'", summary)
	}

	// Update (upsert)
	saveSummary(db, "/tmp/test.jsonl", "hash2", 200, "updated summary")

	db.QueryRow("SELECT summary FROM sessions WHERE path = ?", "/tmp/test.jsonl").Scan(&summary)
	if summary != "updated summary" {
		t.Errorf("summary = %q, want 'updated summary'", summary)
	}
}

func TestCheckSessions_NewFile(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db, _ := openDB()
	defer db.Close()

	f := filepath.Join(dir, "session.jsonl")
	os.WriteFile(f, []byte(`{"type":"test"}`), 0644)

	files := []sessionFile{{source: "test", path: f}}
	states := checkSessions(db, files)

	if len(states) != 1 {
		t.Fatalf("expected 1 state, got %d", len(states))
	}
	if !states[0].changed {
		t.Error("new file should be marked changed")
	}
	if states[0].hash == "" {
		t.Error("hash should not be empty")
	}
}

func TestCheckSessions_UnchangedFile(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db, _ := openDB()
	defer db.Close()

	f := filepath.Join(dir, "session.jsonl")
	os.WriteFile(f, []byte(`{"type":"test"}`), 0644)

	files := []sessionFile{{source: "test", path: f}}

	// First check
	states := checkSessions(db, files)
	saveSummary(db, f, states[0].hash, states[0].size, "test summary")

	// Second check — should be unchanged
	states2 := checkSessions(db, files)
	if states2[0].changed {
		t.Error("file should be unchanged after saving summary")
	}
	if states2[0].summary != "test summary" {
		t.Errorf("summary = %q", states2[0].summary)
	}
}

func TestCheckSessions_ModifiedFile(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db, _ := openDB()
	defer db.Close()

	f := filepath.Join(dir, "session.jsonl")
	os.WriteFile(f, []byte(`{"type":"v1"}`), 0644)

	files := []sessionFile{{source: "test", path: f}}
	states := checkSessions(db, files)
	saveSummary(db, f, states[0].hash, states[0].size, "v1 summary")

	// Modify file
	os.WriteFile(f, []byte(`{"type":"v2","extra":"data"}`), 0644)
	states2 := checkSessions(db, files)
	if !states2[0].changed {
		t.Error("modified file should be marked changed")
	}
}

func TestCheckSessions_NonExistentFile(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db, _ := openDB()
	defer db.Close()

	files := []sessionFile{{source: "test", path: "/nonexistent.jsonl"}}
	states := checkSessions(db, files)
	if len(states) != 0 {
		t.Errorf("expected 0 states for nonexistent file, got %d", len(states))
	}
}

func TestImportSummaries(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	origSD := sessionDir
	sessionDir = dir
	defer func() {
		dbPath = origDB
		sessionDir = origSD
	}()

	// Create a file that's referenced in summaries
	testFile := filepath.Join(dir, "real.jsonl")
	os.WriteFile(testFile, []byte(`{"type":"test"}`), 0644)

	// Write summaries file
	summaries := `[{"path":"` + testFile + `","summary":"imported summary"}]`
	os.WriteFile(filepath.Join(dir, "summaries.json"), []byte(summaries), 0644)

	importSummaries()

	// Verify import
	db, _ := sql.Open("sqlite", dbPath)
	defer db.Close()
	var summary string
	db.QueryRow("SELECT summary FROM sessions WHERE path = ?", testFile).Scan(&summary)
	if summary != "imported summary" {
		t.Errorf("imported summary = %q", summary)
	}
}

func TestImportSummaries_NoFile(t *testing.T) {
	origSD := sessionDir
	sessionDir = t.TempDir()
	defer func() { sessionDir = origSD }()

	// Should not panic when no summaries file
	importSummaries()
}

func TestImportSummaries_BadJSON(t *testing.T) {
	origSD := sessionDir
	sessionDir = t.TempDir()
	defer func() { sessionDir = origSD }()

	os.WriteFile(filepath.Join(sessionDir, "summaries.json"), []byte("not json"), 0644)
	importSummaries() // should not panic
}

func TestImportSummaries_EmptyArray(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	origSD := sessionDir
	sessionDir = dir
	defer func() {
		dbPath = origDB
		sessionDir = origSD
	}()

	os.WriteFile(filepath.Join(dir, "summaries.json"), []byte("[]"), 0644)
	importSummaries() // should not panic
}

func TestHandleDB_Stats(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	// Just verify no panic
	handleDB([]string{"stats"})
}

func TestHandleDB_List(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	handleDB([]string{"list"})
}

func TestHandleDB_Empty(t *testing.T) {
	handleDB(nil)
}

func TestHandleDB_Unknown(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	handleDB([]string{"unknown"})
}

func TestHandleDB_GC_NoStale(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db, _ := openDB()
	// Add a session that exists on disk
	f := filepath.Join(dir, "exists.jsonl")
	os.WriteFile(f, []byte(`{"type":"test"}`), 0644)
	saveSummary(db, f, "hash", 15, "exists")
	db.Close()

	handleDB([]string{"gc"}) // should report no cleanup needed
}

// ── Pattern Cultivation Tests ──

func TestUpsertPattern_NewAndUpdate(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db, _ := openDB()
	defer db.Close()

	// Insert new
	upsertPattern(db, patternInput{Name: "k8s-health", Description: "check cluster", Example: "kubectl get nodes", Source: "/tmp/s1.jsonl"})

	var count, seen int
	var name, status string
	db.QueryRow("SELECT name, seen_count, status FROM patterns WHERE name = 'k8s-health'").Scan(&name, &seen, &status)
	if seen != 1 || status != "candidate" {
		t.Errorf("expected seen=1 status=candidate, got seen=%d status=%s", seen, status)
	}

	// Update (upsert same name, different source)
	upsertPattern(db, patternInput{Name: "k8s-health", Source: "/tmp/s2.jsonl"})
	db.QueryRow("SELECT seen_count FROM patterns WHERE name = 'k8s-health'").Scan(&seen)
	if seen != 2 {
		t.Errorf("expected seen=2, got %d", seen)
	}

	// Check source diversity
	var sourcesJSON string
	db.QueryRow("SELECT sources FROM patterns WHERE name = 'k8s-health'").Scan(&sourcesJSON)
	var sources []string
	json.Unmarshal([]byte(sourcesJSON), &sources)
	if len(sources) != 2 {
		t.Errorf("expected 2 sources, got %d", len(sources))
	}

	// Dedup same source
	upsertPattern(db, patternInput{Name: "k8s-health", Source: "/tmp/s1.jsonl"})
	db.QueryRow("SELECT sources FROM patterns WHERE name = 'k8s-health'").Scan(&sourcesJSON)
	json.Unmarshal([]byte(sourcesJSON), &sources)
	if len(sources) != 2 {
		t.Errorf("expected 2 sources after dedup, got %d", len(sources))
	}

	db.QueryRow("SELECT COUNT(*) FROM patterns").Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 pattern, got %d", count)
	}
}

func TestPatternFeedback(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db, _ := openDB()
	defer db.Close()

	upsertPattern(db, patternInput{Name: "test-pat", Description: "test", Source: "/tmp/s.jsonl"})

	var pid int
	db.QueryRow("SELECT id FROM patterns WHERE name = 'test-pat'").Scan(&pid)

	now := "2026-04-05T00:00:00Z"
	db.Exec("INSERT INTO pattern_feedback (pattern_id, outcome, note, session, created_at) VALUES (?, 'success', '', '', ?)", pid, now)
	db.Exec("INSERT INTO pattern_feedback (pattern_id, outcome, note, session, created_at) VALUES (?, 'success', '', '', ?)", pid, now)
	db.Exec("INSERT INTO pattern_feedback (pattern_id, outcome, note, session, created_at) VALUES (?, 'failure', '', '', ?)", pid, now)

	var sources []string
	json.Unmarshal([]byte(`["/tmp/s.jsonl"]`), &sources)
	m := getPatternMetrics(db, pid, sources)

	if m.SuccessCount != 2 {
		t.Errorf("success=%d, want 2", m.SuccessCount)
	}
	if m.FailureCount != 1 {
		t.Errorf("failure=%d, want 1", m.FailureCount)
	}
	// reliability = 2/(2+1) ≈ 0.667
	if m.Reliability < 0.66 || m.Reliability > 0.67 {
		t.Errorf("reliability=%.3f, want ~0.667", m.Reliability)
	}
}

func TestCultivate_NotReady(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db, _ := openDB()
	defer db.Close()

	// Pattern with only 2 observations — below threshold
	upsertPattern(db, patternInput{Name: "too-young", Description: "not ready", Source: "/tmp/a.jsonl"})
	upsertPattern(db, patternInput{Name: "too-young", Source: "/tmp/b.jsonl"})

	handleCultivate(db, false) // should promote nothing

	var status string
	db.QueryRow("SELECT status FROM patterns WHERE name = 'too-young'").Scan(&status)
	if status != "candidate" {
		t.Errorf("status=%s, want candidate (not promoted)", status)
	}
}

func TestCultivate_Promotes(t *testing.T) {
	origDB := dbPath
	origHome := home
	origWeiranHome := appHome
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	home = dir
	appHome = filepath.Join(dir, ".openclaw")
	defer func() {
		dbPath = origDB
		home = origHome
		appHome = origWeiranHome
	}()

	db, _ := openDB()
	defer db.Close()

	// Create pattern with enough observations
	for i := 0; i < 6; i++ {
		upsertPattern(db, patternInput{
			Name:        "mature-skill",
			Description: "A well-tested pattern",
			Example:     "do the thing",
			Source:      filepath.Join(dir, fmt.Sprintf("s%d.jsonl", i%3)), // 3 unique sources
		})
	}

	var pid int
	db.QueryRow("SELECT id FROM patterns WHERE name = 'mature-skill'").Scan(&pid)

	// Add positive feedback
	now := "2026-04-05T00:00:00Z"
	for i := 0; i < 5; i++ {
		db.Exec("INSERT INTO pattern_feedback (pattern_id, outcome, note, session, created_at) VALUES (?, 'success', '', '', ?)", pid, now)
	}

	handleCultivate(db, false)

	// Check promoted
	var status string
	db.QueryRow("SELECT status FROM patterns WHERE name = 'mature-skill'").Scan(&status)
	if status != "promoted" {
		t.Errorf("status=%s, want promoted", status)
	}

	// Check SKILL.md was created
	skillPath := filepath.Join(dir, ".openclaw", "skills", "mature-skill", "SKILL.md")
	if _, err := os.Stat(skillPath); os.IsNotExist(err) {
		t.Errorf("SKILL.md not created at %s", skillPath)
	}
}

func TestGenerateSkillMD(t *testing.T) {
	m := patternMetrics{Reliability: 0.9, NegativeRate: 0.1, Diversity: 3}
	content := generateSkillMD("test-skill", "A test skill", "run test", []string{"/a.jsonl", "/b.jsonl"}, 7, m)

	if !strings.Contains(content, "name: test-skill") {
		t.Error("missing name in frontmatter")
	}
	if !strings.Contains(content, "cultivated: true") {
		t.Error("missing cultivated flag")
	}
	if !strings.Contains(content, "A test skill") {
		t.Error("missing description")
	}
	if !strings.Contains(content, "run test") {
		t.Error("missing example")
	}
}

func TestPatternReject(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db, _ := openDB()
	defer db.Close()

	upsertPattern(db, patternInput{Name: "bad-pat", Description: "reject me", Source: "/tmp/x.jsonl"})
	db.Exec("UPDATE patterns SET status = 'rejected' WHERE name = 'bad-pat'")

	var status string
	db.QueryRow("SELECT status FROM patterns WHERE name = 'bad-pat'").Scan(&status)
	if status != "rejected" {
		t.Errorf("status=%s, want rejected", status)
	}
}

func TestHandleDB_GC_WithStale(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	db, _ := openDB()
	// Add a session pointing to nonexistent file
	saveSummary(db, "/tmp/nonexistent-session-xyz.jsonl", "hash", 100, "stale")
	// Add one that exists
	f := filepath.Join(dir, "exists.jsonl")
	os.WriteFile(f, []byte(`{"type":"test"}`), 0644)
	saveSummary(db, f, "hash", 15, "exists")
	db.Close()

	handleDB([]string{"gc"})

	// Verify stale was removed
	db2, _ := openDB()
	defer db2.Close()
	var count int
	db2.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 remaining session, got %d", count)
	}
}
