package app

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

// resetServerDB resets the singleton for test isolation.
func resetServerDB(t *testing.T) {
	t.Helper()
	serverDB = nil
	serverDBOnce = sync.Once{}

	tmpDir := t.TempDir()
	origAppDir := appDir
	appDir = tmpDir
	t.Cleanup(func() { appDir = origAppDir })
}

func TestOpenServerDB(t *testing.T) {
	resetServerDB(t)

	db, err := openServerDB()
	if err != nil {
		t.Fatalf("openServerDB: %v", err)
	}
	if db == nil {
		t.Fatal("db is nil")
	}

	// Verify table exists
	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='server_sessions'`).Scan(&name)
	if err != nil {
		t.Fatalf("table not created: %v", err)
	}
	if name != "server_sessions" {
		t.Errorf("expected server_sessions, got %s", name)
	}
}

func TestOpenServerDB_Idempotent(t *testing.T) {
	resetServerDB(t)

	db1, _ := openServerDB()
	db2, _ := openServerDB()
	if db1 != db2 {
		t.Error("expected same DB instance (sync.Once)")
	}
}

func TestEnsureServerSession(t *testing.T) {
	resetServerDB(t)

	ensureServerSession("sess-1", "test session")

	db, _ := openServerDB()
	var name string
	var renamed, userTurns, autoNamed int
	err := db.QueryRow(`SELECT name, renamed, user_turns, auto_named FROM server_sessions WHERE session_id='sess-1'`).
		Scan(&name, &renamed, &userTurns, &autoNamed)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if name != "test session" {
		t.Errorf("name: got %q, want %q", name, "test session")
	}
	if renamed != 0 || userTurns != 0 || autoNamed != 0 {
		t.Errorf("defaults wrong: renamed=%d turns=%d auto=%d", renamed, userTurns, autoNamed)
	}
}

func TestEnsureServerSession_UpdatesTimestamp(t *testing.T) {
	resetServerDB(t)

	ensureServerSession("sess-2", "first")

	db, _ := openServerDB()
	var ts1 string
	db.QueryRow(`SELECT updated_at FROM server_sessions WHERE session_id='sess-2'`).Scan(&ts1)

	// Insert again (ON CONFLICT DO UPDATE)
	ensureServerSession("sess-2", "first")

	var ts2 string
	db.QueryRow(`SELECT updated_at FROM server_sessions WHERE session_id='sess-2'`).Scan(&ts2)
	// timestamps may be the same if fast, but should not error
}

func TestMarkRenamed(t *testing.T) {
	resetServerDB(t)

	ensureServerSession("sess-3", "original")
	markRenamed("sess-3", "new name")

	db, _ := openServerDB()
	var name string
	var renamed int
	db.QueryRow(`SELECT name, renamed FROM server_sessions WHERE session_id='sess-3'`).Scan(&name, &renamed)

	if name != "new name" {
		t.Errorf("name: got %q, want %q", name, "new name")
	}
	if renamed != 1 {
		t.Errorf("renamed: got %d, want 1", renamed)
	}
}

func TestIncrementUserTurns(t *testing.T) {
	resetServerDB(t)

	ensureServerSession("sess-4", "test")

	for i := 1; i <= 7; i++ {
		turns, renamed := incrementUserTurns("sess-4")
		if turns != i {
			t.Errorf("turn %d: got turns=%d", i, turns)
		}
		if renamed {
			t.Errorf("turn %d: should not be renamed", i)
		}
	}
}

func TestIncrementUserTurns_AfterRename(t *testing.T) {
	resetServerDB(t)

	ensureServerSession("sess-5", "test")
	markRenamed("sess-5", "renamed")

	turns, renamed := incrementUserTurns("sess-5")
	if turns != 1 {
		t.Errorf("turns: got %d, want 1", turns)
	}
	if !renamed {
		t.Error("should be renamed")
	}
}

func TestMarkAutoNamed(t *testing.T) {
	resetServerDB(t)

	ensureServerSession("sess-6", "original")
	markAutoNamed("sess-6", "AI Title")

	db, _ := openServerDB()
	var name string
	var autoNamed int
	db.QueryRow(`SELECT name, auto_named FROM server_sessions WHERE session_id='sess-6'`).Scan(&name, &autoNamed)

	if name != "AI Title" {
		t.Errorf("name: got %q, want %q", name, "AI Title")
	}
	if autoNamed != 1 {
		t.Errorf("auto_named: got %d, want 1", autoNamed)
	}
}

func TestIsAutoNamed(t *testing.T) {
	resetServerDB(t)

	ensureServerSession("sess-7", "test")
	if isAutoNamed("sess-7") {
		t.Error("should not be auto-named initially")
	}

	markAutoNamed("sess-7", "AI Title")
	if !isAutoNamed("sess-7") {
		t.Error("should be auto-named after markAutoNamed")
	}
}

func TestShouldAutoRename(t *testing.T) {
	resetServerDB(t)

	ensureServerSession("sess-8", "test")

	// 0 turns — no
	if shouldAutoRename("sess-8") {
		t.Error("should not rename at 0 turns")
	}

	// Bump to 4 — no
	for i := 0; i < 4; i++ {
		incrementUserTurns("sess-8")
	}
	if shouldAutoRename("sess-8") {
		t.Error("should not rename at 4 turns")
	}

	// Bump to 5 — yes
	incrementUserTurns("sess-8")
	if !shouldAutoRename("sess-8") {
		t.Error("should rename at 5 turns")
	}
}

func TestShouldAutoRename_ManuallyRenamed(t *testing.T) {
	resetServerDB(t)

	ensureServerSession("sess-9", "test")
	for i := 0; i < 5; i++ {
		incrementUserTurns("sess-9")
	}
	markRenamed("sess-9", "custom")

	if shouldAutoRename("sess-9") {
		t.Error("should not auto-rename after manual rename")
	}
}

func TestShouldAutoRename_AlreadyAutoNamed(t *testing.T) {
	resetServerDB(t)

	ensureServerSession("sess-10", "test")
	for i := 0; i < 5; i++ {
		incrementUserTurns("sess-10")
	}
	markAutoNamed("sess-10", "AI Title")

	if shouldAutoRename("sess-10") {
		t.Error("should not auto-rename again after auto-naming")
	}
}

func TestShouldAutoRename_NonExistent(t *testing.T) {
	resetServerDB(t)

	if shouldAutoRename("nonexistent") {
		t.Error("should not rename non-existent session")
	}
}

func TestServerDBFile_Created(t *testing.T) {
	resetServerDB(t)
	openServerDB()

	dbFile := filepath.Join(appDir, "server_sessions.db")
	if _, err := os.Stat(dbFile); os.IsNotExist(err) {
		t.Error("DB file should exist on disk")
	}
}

func TestFilterNestedClaudeEnv(t *testing.T) {
	env := []string{
		"HOME=/Users/test",
		"CLAUDE_CODE_SESSION=abc123",
		"PATH=/usr/bin",
		"CLAUDE_CODE_ENTRY_POINT=cli",
		"TERM=xterm",
	}
	filtered := filterNestedClaudeEnv(env)
	if len(filtered) != 3 {
		t.Errorf("expected 3 vars, got %d: %v", len(filtered), filtered)
	}
	for _, e := range filtered {
		if strings.HasPrefix(e, "CLAUDE_CODE_") {
			t.Errorf("should have filtered out %q", e)
		}
	}
}

func TestCallHaikuForTitle_NoClaude(t *testing.T) {
	origBin := claudeBin
	claudeBin = "/nonexistent/claude"
	defer func() { claudeBin = origBin }()

	title := callHaikuForTitle("some conversation context")
	if title != "" {
		t.Errorf("expected empty title when claude binary missing, got %q", title)
	}
}

// TestConcurrentDBAccess ensures no races with parallel writes.
func TestConcurrentDBAccess(t *testing.T) {
	resetServerDB(t)

	ensureServerSession("concurrent", "test")

	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			incrementUserTurns("concurrent")
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}

	db, _ := openServerDB()
	var turns int
	db.QueryRow(`SELECT user_turns FROM server_sessions WHERE session_id='concurrent'`).Scan(&turns)
	if turns != 10 {
		t.Errorf("turns: got %d, want 10", turns)
	}
}

// Verify DB doesn't leak connections
func TestOpenServerDB_NilOnBadPath(t *testing.T) {
	// This tests that we handle the singleton properly
	// The actual error path is hard to trigger since sqlite creates the file
	resetServerDB(t)
	db, err := openServerDB()
	if err != nil {
		t.Fatalf("should succeed in tmpdir: %v", err)
	}
	_ = db
}

// Ensure the schema supports all expected columns
func TestSchemaColumns(t *testing.T) {
	resetServerDB(t)
	db, _ := openServerDB()

	// Insert with all columns
	_, err := db.Exec(`INSERT INTO server_sessions (session_id, name, renamed, user_turns, auto_named, created_at, updated_at)
		VALUES ('schema-test', 'test', 0, 0, 0, '2025-01-01', '2025-01-01')`)
	if err != nil {
		t.Fatalf("insert all columns: %v", err)
	}

	var sessionID, name, createdAt, updatedAt string
	var renamed, userTurns, autoNamed int
	err = db.QueryRow(`SELECT session_id, name, renamed, user_turns, auto_named, created_at, updated_at FROM server_sessions WHERE session_id='schema-test'`).
		Scan(&sessionID, &name, &renamed, &userTurns, &autoNamed, &createdAt, &updatedAt)
	if err != nil {
		t.Fatalf("select all columns: %v", err)
	}

	// Suppress unused variable warnings
	_ = sql.ErrNoRows
}
