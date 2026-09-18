package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindWeiranTmpDirs(t *testing.T) {
	// Create some test dirs in the actual tmp dir
	dir1 := filepath.Join(os.TempDir(), agentName+"-test-0001")
	dir2 := filepath.Join(os.TempDir(), agentName+"-test-0002")
	os.MkdirAll(dir1, 0700)
	os.MkdirAll(dir2, 0700)
	defer os.RemoveAll(dir1)
	defer os.RemoveAll(dir2)

	dirs := findAppTmpDirs()
	found := 0
	for _, d := range dirs {
		if d == dir1 || d == dir2 {
			found++
		}
	}
	if found < 2 {
		t.Errorf("expected to find at least 2 test dirs, found %d", found)
	}
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("world!"), 0644)
	sub := filepath.Join(dir, "sub")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "c.txt"), []byte("nested"), 0644)

	size := dirSize(dir)
	// "hello"(5) + "world!"(6) + "nested"(6) = 17
	if size != 17 {
		t.Errorf("dirSize = %d, want 17", size)
	}
}

func TestDirSize_Empty(t *testing.T) {
	dir := t.TempDir()
	size := dirSize(dir)
	if size != 0 {
		t.Errorf("dirSize empty = %d, want 0", size)
	}
}

func TestDirSize_NonExistent(t *testing.T) {
	size := dirSize("/nonexistent/dir/path")
	if size != 0 {
		t.Errorf("dirSize nonexistent = %d, want 0", size)
	}
}

func TestMustRead(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.txt")
	os.WriteFile(f, []byte("content"), 0644)

	data := mustRead(f)
	if string(data) != "content" {
		t.Errorf("mustRead = %q, want 'content'", data)
	}
}

func TestMustRead_NonExistent(t *testing.T) {
	data := mustRead("/nonexistent/file")
	if len(data) != 0 {
		t.Errorf("mustRead nonexistent should return empty, got %d bytes", len(data))
	}
}

func TestHandleConfig_NoPanic(t *testing.T) {
	// Just verify it doesn't panic
	handleConfig()
}

func TestInitSessionDir(t *testing.T) {
	origDir := sessionDir
	origOut := promptOut
	defer func() {
		sessionDir = origDir
		promptOut = origOut
	}()

	initSessionDir()

	if sessionDir == "" {
		t.Error("sessionDir should be set")
	}
	if promptOut == "" {
		t.Error("promptOut should be set")
	}
	if _, err := os.Stat(sessionDir); err != nil {
		t.Errorf("sessionDir should exist: %v", err)
	}
	// cleanup
	os.RemoveAll(sessionDir)
}

func TestSessionTmp(t *testing.T) {
	origDir := sessionDir
	sessionDir = "/tmp/test-session"
	defer func() { sessionDir = origDir }()

	got := sessionTmp("report.txt")
	if got != "/tmp/test-session/report.txt" {
		t.Errorf("sessionTmp = %q", got)
	}
}

func TestPrintLastRuns_NoMetrics(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	// Should not panic with no metrics file
	printLastRuns()
}

func TestPrintLastRuns_WithMetrics(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	metricsPath := filepath.Join(dir, "metrics.jsonl")
	data := `{"ts":"2026-04-05T10:00:00-07:00","mode":"heartbeat","exit_code":0,"duration_s":45.2}
{"ts":"2026-04-05T11:00:00-07:00","mode":"cron","exit_code":1,"duration_s":120.5}
`
	os.WriteFile(metricsPath, []byte(data), 0644)

	// Should not panic
	printLastRuns()
}

func TestPrintMetricsSummary_NoFile(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	printMetricsSummary()
}

func TestPrintMetricsSummary_WithData(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	metricsPath := filepath.Join(dir, "metrics.jsonl")
	data := `{"ts":"2026-04-05T10:00:00Z","mode":"heartbeat","exit_code":0,"duration_s":45.2}
{"ts":"2026-04-05T11:00:00Z","mode":"heartbeat","exit_code":0,"duration_s":50.0}
{"ts":"2026-04-05T12:00:00Z","mode":"cron","exit_code":1,"duration_s":120.5}
`
	os.WriteFile(metricsPath, []byte(data), 0644)

	printMetricsSummary()
}
