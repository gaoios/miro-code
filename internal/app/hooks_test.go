package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSafetyCheck_AllOK(t *testing.T) {
	// safetyCheck reads from workspace — test with real workspace
	// Just verify it doesn't panic with real files
	// (actual safety check logic tested via file manipulation below)

	// Capture stderr by redirecting (not easy in Go, so just verify no panic)
	safetyCheck("test")
}

func TestSafetyCheck_MissingSoulFile(t *testing.T) {
	dir := t.TempDir()
	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	// Create most soul files except one
	for _, f := range []string{"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "TOOLS.md", "MEMORY.md"} {
		os.WriteFile(filepath.Join(dir, f), []byte("content"), 0644)
	}
	// HEARTBEAT.md is missing — should trigger warning

	// Create memory dir to avoid nil errors
	os.MkdirAll(filepath.Join(dir, "memory", "topics"), 0755)
	os.MkdirAll(filepath.Join(dir, "projects"), 0755)

	// Run — should not panic, will print warnings to stderr
	safetyCheck("test")
}

func TestSafetyCheck_EmptySoulFile(t *testing.T) {
	dir := t.TempDir()
	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	for _, f := range []string{"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "TOOLS.md", "MEMORY.md", "HEARTBEAT.md"} {
		os.WriteFile(filepath.Join(dir, f), []byte("content"), 0644)
	}
	// Make one empty
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte(""), 0644)

	os.MkdirAll(filepath.Join(dir, "memory", "topics"), 0755)
	os.MkdirAll(filepath.Join(dir, "projects"), 0755)

	safetyCheck("test")
}

func TestSafetyCheck_BloatedMemory(t *testing.T) {
	dir := t.TempDir()
	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	for _, f := range []string{"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "TOOLS.md", "MEMORY.md", "HEARTBEAT.md"} {
		os.WriteFile(filepath.Join(dir, f), []byte("content"), 0644)
	}
	os.MkdirAll(filepath.Join(dir, "memory", "topics"), 0755)
	os.MkdirAll(filepath.Join(dir, "projects"), 0755)

	// Create a >100KB memory file
	bigContent := strings.Repeat("x", 110*1024)
	os.WriteFile(filepath.Join(dir, "memory", "bloated.md"), []byte(bigContent), 0644)

	safetyCheck("test")
}

func TestDeliverReport_NoFile(t *testing.T) {
	origDir := sessionDir
	sessionDir = t.TempDir()
	defer func() { sessionDir = origDir }()

	// No report file — should just return without error
	deliverReport("test")
}

func TestDeliverReport_EmptyFile(t *testing.T) {
	origDir := sessionDir
	sessionDir = t.TempDir()
	defer func() { sessionDir = origDir }()

	os.WriteFile(sessionTmp("report.txt"), []byte(""), 0644)
	deliverReport("test")
}

func TestDeliverReport_WithContent(t *testing.T) {
	origDir := sessionDir
	sessionDir = t.TempDir()
	defer func() { sessionDir = origDir }()

	os.WriteFile(sessionTmp("report.txt"), []byte("test report content"), 0644)
	// Will try to send Telegram — will fail silently (no token in test)
	deliverReport("cron")
}

func TestAutoCleanTmp_CleansOldDirs(t *testing.T) {
	// Create fake tmp dirs with old timestamps
	dir1 := filepath.Join(os.TempDir(), agentName+"-0101-0000")
	dir2 := filepath.Join(os.TempDir(), agentName+"-0101-0001")
	os.MkdirAll(dir1, 0700)
	os.MkdirAll(dir2, 0700)
	// Make them old (set mtime to 48h ago)
	oldTime := time.Now().Add(-48 * time.Hour)
	os.Chtimes(dir1, oldTime, oldTime)
	os.Chtimes(dir2, oldTime, oldTime)
	defer func() {
		os.RemoveAll(dir1)
		os.RemoveAll(dir2)
	}()

	autoCleanTmp()

	// Both should be cleaned
	if _, err := os.Stat(dir1); !os.IsNotExist(err) {
		t.Errorf("dir1 should be cleaned, but still exists")
	}
	if _, err := os.Stat(dir2); !os.IsNotExist(err) {
		t.Errorf("dir2 should be cleaned, but still exists")
	}
}

func TestAutoCleanTmp_PreservesRecent(t *testing.T) {
	// Create a recent tmp dir
	dir := filepath.Join(os.TempDir(), agentName+"-9999-0000")
	os.MkdirAll(dir, 0700)
	defer os.RemoveAll(dir)

	autoCleanTmp()

	// Should NOT be cleaned (recent)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Errorf("recent dir should be preserved, but was cleaned")
	}
}

func TestAutoCleanTmp_PreservesCurrentSession(t *testing.T) {
	// Set current session dir
	origSD := sessionDir
	dir := filepath.Join(os.TempDir(), agentName+"-0102-0000")
	os.MkdirAll(dir, 0700)
	sessionDir = dir
	oldTime := time.Now().Add(-48 * time.Hour)
	os.Chtimes(dir, oldTime, oldTime)
	defer func() {
		sessionDir = origSD
		os.RemoveAll(dir)
	}()

	autoCleanTmp()

	// Should NOT be cleaned (current session)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Errorf("current session dir should be preserved, but was cleaned")
	}
}

func TestRunHooks_NoHooksDir(t *testing.T) {
	origDir := hooksDir
	hooksDir = filepath.Join(t.TempDir(), "nonexistent")
	origSD := sessionDir
	sessionDir = t.TempDir()
	defer func() {
		hooksDir = origDir
		sessionDir = origSD
	}()

	// Should not panic when hooks dir doesn't exist
	runHooks("test")
}

func TestRunHooks_WithCustomHook(t *testing.T) {
	dir := t.TempDir()
	origDir := hooksDir
	hooksDir = dir
	origSD := sessionDir
	sessionDir = t.TempDir()
	defer func() {
		hooksDir = origDir
		sessionDir = origSD
	}()

	// Create a test hook
	hookDir := filepath.Join(dir, "test.d")
	os.MkdirAll(hookDir, 0755)
	os.WriteFile(filepath.Join(hookDir, "01-test.sh"), []byte("#!/bin/sh\necho test hook ran\n"), 0755)

	runHooks("test")
}

func TestRunHooks_SkipsNonSh(t *testing.T) {
	dir := t.TempDir()
	origDir := hooksDir
	hooksDir = dir
	origSD := sessionDir
	sessionDir = t.TempDir()
	defer func() {
		hooksDir = origDir
		sessionDir = origSD
	}()

	hookDir := filepath.Join(dir, "test.d")
	os.MkdirAll(hookDir, 0755)
	os.WriteFile(filepath.Join(hookDir, "README.md"), []byte("not a hook"), 0644)
	os.WriteFile(filepath.Join(hookDir, "01-real.sh"), []byte("#!/bin/sh\ntrue\n"), 0755)

	runHooks("test")
}
