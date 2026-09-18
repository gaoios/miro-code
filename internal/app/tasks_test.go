package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestHeartbeatTask(t *testing.T) {
	origSD := sessionDir
	sessionDir = t.TempDir()
	defer func() { sessionDir = origSD }()

	task := heartbeatTask()
	if task == "" {
		t.Fatal("heartbeat task is empty")
	}
	if !strings.Contains(task, "heartbeat patrol") {
		t.Error("missing 'heartbeat patrol' in task text")
	}
	if !strings.Contains(task, "HEARTBEAT.md") {
		t.Error("missing HEARTBEAT.md reference")
	}
	if !strings.Contains(task, "report.txt") {
		t.Error("missing report file instruction")
	}
}

func TestCronTask(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	origSD := sessionDir
	sessionDir = dir
	defer func() {
		dbPath = origDB
		sessionDir = origSD
	}()

	task := cronTask()
	if task == "" {
		t.Fatal("cron task is empty")
	}
	if !strings.Contains(task, "Memory Consolidation") {
		t.Error("missing 'Memory Consolidation' in task text")
	}

	today := time.Now().Format("2006-01-02")
	if !strings.Contains(task, today) {
		t.Errorf("missing today's date %s in task text", today)
	}
}

func TestCronTask_ContainsWeeklyOnSunday(t *testing.T) {
	if time.Now().Weekday() != time.Sunday {
		t.Skip("not Sunday, skip weekly test")
	}

	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	origSD := sessionDir
	sessionDir = dir
	defer func() {
		dbPath = origDB
		sessionDir = origSD
	}()

	task := cronTask()
	if !strings.Contains(task, "Deep Review") {
		t.Error("Sunday cron task should contain 'Deep Review'")
	}
}

func TestWeeklyPreScan(t *testing.T) {
	origSD := sessionDir
	sessionDir = t.TempDir()
	defer func() { sessionDir = origSD }()

	task := weeklyPreScan()
	if task == "" {
		t.Fatal("weekly pre-scan task is empty")
	}
	if !strings.Contains(task, "Pre-Scan") {
		t.Error("missing 'Pre-Scan' in task text")
	}
	if !strings.Contains(task, "weekly-scan.md") {
		t.Error("missing weekly-scan.md output instruction")
	}
}

// TestEvolveTaskAwkExtractsSummaryJSON guards against a regression where the
// awk pipeline used to persist the cycle-summary JSON to SQLite never
// flipped its `f` flag on, so it produced empty output and `db evolve-log -`
// failed with the usage error. The evolve cycle would silently lose its
// summary row.
func TestEvolveTaskAwkExtractsSummaryJSON(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("awk not available")
	}
	task := evolveTask()

	// Pull the awk command out of the rendered task text. It's the only
	// `awk '...'` line in the template.
	re := regexp.MustCompile(`awk '([^']+)'`)
	m := re.FindStringSubmatch(task)
	if m == nil {
		t.Fatal("could not find awk command in evolve task text")
	}
	awkScript := m[1]

	// Synthesize a minimal report with the same structure as the template.
	report := strings.Join([]string{
		"🧬 Evolution report (2026-04-27):",
		"",
		"## Summary (structured — for automation)",
		"```json",
		`{"cycle_date":"2026-04-27","status":"evolved"}`,
		"```",
		"",
		"## Narrative",
		"- nothing to see here",
		"```json",
		`{"this":"should","not":"be","extracted":true}`,
		"```",
		"",
	}, "\n")

	tmp := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(tmp, []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("awk", awkScript, tmp).Output()
	if err != nil {
		t.Fatalf("awk failed: %v", err)
	}
	got := strings.TrimSpace(string(out))
	want := `{"cycle_date":"2026-04-27","status":"evolved"}`
	if got != want {
		t.Fatalf("awk extraction wrong:\nwant: %s\ngot:  %s", want, got)
	}
}

func TestWeeklyPreScan_ListsFiles(t *testing.T) {
	origSD := sessionDir
	sessionDir = t.TempDir()
	defer func() { sessionDir = origSD }()

	// Create a today daily note for it to find
	today := time.Now().Format("2006-01-02")
	noteDir := filepath.Join(workspace, "memory")
	notePath := filepath.Join(noteDir, today+".md")

	// Check if the file actually exists (it should on Kiyor's machine)
	if _, err := os.Stat(notePath); err != nil {
		t.Skip("no today's daily note, skip file listing test")
	}

	task := weeklyPreScan()
	if !strings.Contains(task, today) {
		t.Errorf("should reference today's note %s", today)
	}
}
