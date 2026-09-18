package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupFTSTest creates a temp DB and a fake workspace/memory directory,
// returns a cleanup function. Used by all FTS tests for isolation.
func setupFTSTest(t *testing.T) func() {
	t.Helper()
	origDB := dbPath
	origWS := workspace
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	workspace = dir
	memDir := filepath.Join(dir, "memory", "daily")
	os.MkdirAll(memDir, 0755)
	return func() {
		dbPath = origDB
		workspace = origWS
	}
}

func TestEnsureFTSSchemas(t *testing.T) {
	defer setupFTSTest(t)()

	db, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	// Verify daily_notes table exists
	var name string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='daily_notes'`).Scan(&name); err != nil {
		t.Fatalf("daily_notes table missing: %v", err)
	}

	// Verify daily_notes_fts virtual table exists
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='daily_notes_fts'`).Scan(&name); err != nil {
		t.Fatalf("daily_notes_fts missing: %v", err)
	}

	// Verify session_summaries_fts virtual table exists
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='session_summaries_fts'`).Scan(&name); err != nil {
		t.Fatalf("session_summaries_fts missing: %v", err)
	}

	// Triggers
	for _, trig := range []string{"daily_notes_ai", "daily_notes_au", "daily_notes_ad", "sessions_ai", "sessions_au", "sessions_ad"} {
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='trigger' AND name=?`, trig).Scan(&name); err != nil {
			t.Errorf("trigger %s missing: %v", trig, err)
		}
	}
}

func TestIndexDailyNotes_Basic(t *testing.T) {
	defer setupFTSTest(t)()

	// Create 3 fake daily notes
	memDir := filepath.Join(workspace, "memory", "daily")
	files := map[string]string{
		"2026-04-01.md": "# Day 1\n\n在 Venice Beach 散步, 和 Kiyor 聊 GLM 5.1 的切换.",
		"2026-04-02.md": "# Day 2\n\n心跳巡检 #100, 服务正常, backlog 清零.",
		"2026-04-03.md": "---\ntitle: Day 3\n---\n\n# Day 3\n\n未然写了一个 FTS5 的故事. in04 highspeed 配置更新.",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(memDir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Non-daily files should be ignored
	os.WriteFile(filepath.Join(memDir, "heartbeat-state.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(memDir, "NOTMD.txt"), []byte("ignored"), 0644)

	added, skipped, err := indexDailyNotes()
	if err != nil {
		t.Fatalf("indexDailyNotes: %v", err)
	}
	if added != 3 {
		t.Errorf("added = %d, want 3", added)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}

	// Verify content was persisted (and frontmatter stripped for 04-03)
	db, _ := openDB()
	defer db.Close()
	var content string
	db.QueryRow(`SELECT content FROM daily_notes WHERE date = ?`, "2026-04-03").Scan(&content)
	if strings.Contains(content, "title: Day 3") {
		t.Errorf("frontmatter not stripped: %q", content)
	}
	if !strings.Contains(content, "未然写了一个 FTS5 的故事") {
		t.Errorf("body missing: %q", content)
	}
}

func TestIndexDailyNotes_Incremental(t *testing.T) {
	defer setupFTSTest(t)()

	memDir := filepath.Join(workspace, "memory", "daily")
	file := filepath.Join(memDir, "2026-04-09.md")
	os.WriteFile(file, []byte("original content"), 0644)

	// Bump mtime to something old enough to be stable under rapid tests
	oldTime := time.Now().Add(-10 * time.Minute)
	os.Chtimes(file, oldTime, oldTime)

	// First index
	added, _, _ := indexDailyNotes()
	if added != 1 {
		t.Fatalf("first pass added=%d, want 1", added)
	}

	// Second index — unchanged file should be skipped
	added2, skipped2, _ := indexDailyNotes()
	if added2 != 0 || skipped2 != 1 {
		t.Errorf("second pass added=%d skipped=%d, want 0/1", added2, skipped2)
	}

	// Modify file
	os.WriteFile(file, []byte("NEW content"), 0644)
	added3, _, _ := indexDailyNotes()
	if added3 != 1 {
		t.Errorf("after modify added=%d, want 1", added3)
	}
}

func TestSearchFTS_Daily(t *testing.T) {
	defer setupFTSTest(t)()

	memDir := filepath.Join(workspace, "memory", "daily")
	os.WriteFile(filepath.Join(memDir, "2026-04-01.md"),
		[]byte("心跳巡检 #200, GLM 5.1 切换成功. in04 highspeed 配置生效."), 0644)
	os.WriteFile(filepath.Join(memDir, "2026-04-02.md"),
		[]byte("Venice Beach 散步, 未然和 Kiyor 聊 alignment faking."), 0644)
	os.WriteFile(filepath.Join(memDir, "2026-04-03.md"),
		[]byte("FTS5 集成完成, 关键词搜索速度很快."), 0644)

	if _, _, err := indexDailyNotes(); err != nil {
		t.Fatalf("index: %v", err)
	}

	// Search "GLM" — should hit day 1
	hits, err := searchFTS("GLM", "daily", 10)
	if err != nil {
		t.Fatalf("search GLM: %v", err)
	}
	if len(hits) != 1 || hits[0].Date != "2026-04-01" {
		t.Errorf("GLM search: got %+v", hits)
	}
	if !strings.Contains(hits[0].Snippet, "[GLM]") {
		t.Errorf("snippet should highlight GLM: %q", hits[0].Snippet)
	}

	// Search "Venice" — should hit day 2
	hits, _ = searchFTS("Venice", "daily", 10)
	if len(hits) != 1 || hits[0].Date != "2026-04-02" {
		t.Errorf("Venice search: got %+v", hits)
	}

	// Search "FTS5" — should hit day 3
	hits, _ = searchFTS("FTS5", "daily", 10)
	if len(hits) != 1 || hits[0].Date != "2026-04-03" {
		t.Errorf("FTS5 search: got %+v", hits)
	}

	// Scope=session — should return zero (no sessions indexed)
	hits, _ = searchFTS("GLM", "session", 10)
	if len(hits) != 0 {
		t.Errorf("session scope should be empty: %+v", hits)
	}

	// Scope=both on unknown term — zero hits, no error
	hits, _ = searchFTS("nonexistentterm12345", "both", 10)
	if len(hits) != 0 {
		t.Errorf("nonexistent match: %+v", hits)
	}
}

func TestSearchFTS_Sessions(t *testing.T) {
	defer setupFTSTest(t)()

	db, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	// Insert test sessions via saveSummary (trigger should populate FTS)
	saveSummary(db, "/tmp/s1.jsonl", "hash1", 100, "Kiyor asked about GLM 5.1 context length")
	saveSummary(db, "/tmp/s2.jsonl", "hash2", 200, "Heartbeat checkin, backlog cleared, no new tickets")
	saveSummary(db, "/tmp/s3.jsonl", "hash3", 300, "Debug the MiniMax highspeed model name format")

	// Search GLM via session scope
	hits, err := searchFTS("GLM", "session", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("GLM hits: got %d, want 1 (hits=%+v)", len(hits), hits)
	}
	if len(hits) > 0 && !strings.Contains(hits[0].Snippet, "[GLM]") {
		t.Errorf("snippet missing highlight: %q", hits[0].Snippet)
	}

	// Update session summary — trigger should re-index
	saveSummary(db, "/tmp/s1.jsonl", "hash1b", 150, "Now talking about opus[1m] variant instead")
	hits, _ = searchFTS("GLM", "session", 10)
	if len(hits) != 0 {
		t.Errorf("after update, GLM should be gone: %+v", hits)
	}
	hits, _ = searchFTS("opus", "session", 10)
	if len(hits) != 1 {
		t.Errorf("opus hits: %+v", len(hits))
	}
}

func TestRebuildFTS(t *testing.T) {
	defer setupFTSTest(t)()

	memDir := filepath.Join(workspace, "memory", "daily")
	os.WriteFile(filepath.Join(memDir, "2026-04-09.md"),
		[]byte("test content for rebuild"), 0644)
	indexDailyNotes()

	// Force corruption: delete from FTS without touching base table
	db, _ := openDB()
	db.Exec(`INSERT INTO daily_notes_fts(daily_notes_fts) VALUES('delete-all')`)
	db.Close()

	// Rebuild should resync
	if err := rebuildFTS(); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	hits, _ := searchFTS("rebuild", "daily", 10)
	if len(hits) != 1 {
		t.Errorf("after rebuild, expect 1 hit, got %d", len(hits))
	}
}

func TestSanitizeFTSQuery(t *testing.T) {
	cases := []struct {
		in    string
		check func(string) bool
		desc  string
	}{
		{"", func(s string) bool { return s == "" }, "empty → empty"},
		{"   ", func(s string) bool { return s == "" }, "whitespace → empty"},
		{"GLM", func(s string) bool { return s == `"GLM"` }, "single english word"},
		{"GLM 5.1", func(s string) bool { return s == `"GLM" "5.1"` }, "english with dot"},
		{`say "hello"`, func(s string) bool { return strings.Contains(s, `"say"`) && strings.Contains(s, "hello") }, "embedded quotes"},
		{"  spaces  everywhere  ", func(s string) bool { return s == `"spaces" "everywhere"` }, "extra spaces trimmed"},
		// Chinese queries get segmented before quoting
		{"心跳", func(s string) bool { return strings.Contains(s, `"心跳"`) }, "chinese word preserved"},
		{"心跳巡检", func(s string) bool {
			return strings.Contains(s, `"心跳"`) && strings.Contains(s, `"巡检"`)
		}, "chinese compound segmented into words"},
		{"#207 巡检", func(s string) bool {
			return strings.Contains(s, `"#207"`) && strings.Contains(s, `"巡检"`)
		}, "mixed punctuation + chinese"},
	}
	for _, c := range cases {
		got := sanitizeFTSQuery(c.in)
		if !c.check(got) {
			t.Errorf("sanitizeFTSQuery(%q) = %q — failed: %s", c.in, got, c.desc)
		}
	}
}

// TestSearchFTS_PunctuationQuery verifies real user queries with dots/hashes
// survive the sanitizer and actually return hits.
func TestSearchFTS_PunctuationQuery(t *testing.T) {
	defer setupFTSTest(t)()

	memDir := filepath.Join(workspace, "memory", "daily")
	os.WriteFile(filepath.Join(memDir, "2026-04-01.md"),
		[]byte("心跳巡检 #207, GLM 5.1 切换成功."), 0644)

	if _, _, err := indexDailyNotes(); err != nil {
		t.Fatalf("index: %v", err)
	}

	for _, q := range []string{"GLM 5.1", "#207", "心跳巡检"} {
		hits, err := searchFTS(q, "daily", 10)
		if err != nil {
			t.Errorf("search %q: %v", q, err)
			continue
		}
		if len(hits) != 1 {
			t.Errorf("search %q: got %d hits, want 1", q, len(hits))
		}
	}
}

func TestStripFrontmatter(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"---\ntitle: x\n---\nbody", "body"},
		{"---\ntitle: x\ntags: [a, b]\n---\n\nbody here", "body here"},
		{"no frontmatter here", "no frontmatter here"},
		{"---\nmalformed", "---\nmalformed"}, // no closing delimiter
	}
	for _, c := range cases {
		got := stripFrontmatter(c.in)
		if got != c.want {
			t.Errorf("stripFrontmatter(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFTSIndexHandlesErrNoRowsAndDBErrors(t *testing.T) {
	cleanup := setupFTSTest(t)
	defer cleanup()

	// Write a daily note
	dailyFile := filepath.Join(workspace, "memory", "daily", "2026-09-18.md")
	if err := os.WriteFile(dailyFile, []byte("Today I tested FTS index error handling"), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. First index when DB has no row yet (ErrNoRows path) -> should insert 1
	added, skipped, err := indexDailyNotes()
	if err != nil {
		t.Fatalf("first indexDailyNotes failed: %v", err)
	}
	if added != 1 || skipped != 0 {
		t.Fatalf("first index: expected added=1, skipped=0; got added=%d, skipped=%d", added, skipped)
	}

	// 2. Second index unchanged file -> should skip 1
	added, skipped, err = indexDailyNotes()
	if err != nil {
		t.Fatalf("second indexDailyNotes failed: %v", err)
	}
	if added != 0 || skipped != 1 {
		t.Fatalf("second index: expected added=0, skipped=1; got added=%d, skipped=%d", added, skipped)
	}
}
