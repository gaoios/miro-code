package app

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// TestFTS5Available is a probe test: verifies that modernc.org/sqlite
// supports FTS5 virtual tables. If this fails, fall back to mattn/go-sqlite3.
func TestFTS5Available(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE VIRTUAL TABLE test_fts USING fts5(content)`); err != nil {
		t.Fatalf("FTS5 CREATE failed: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO test_fts(content) VALUES ('hello world'), ('goodbye world'), ('未然 test 心跳')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Basic MATCH query
	row := db.QueryRow(`SELECT content FROM test_fts WHERE test_fts MATCH ? ORDER BY rank LIMIT 1`, "hello")
	var content string
	if err := row.Scan(&content); err != nil {
		t.Fatalf("MATCH query: %v", err)
	}
	if content != "hello world" {
		t.Fatalf("unexpected match: %q", content)
	}

	// Snippet function test
	var snip string
	row = db.QueryRow(`SELECT snippet(test_fts, 0, '[', ']', '...', 8) FROM test_fts WHERE test_fts MATCH ?`, "world")
	if err := row.Scan(&snip); err != nil {
		t.Fatalf("snippet: %v", err)
	}
	t.Logf("FTS5 OK. snippet=%q", snip)

	// Unicode/CJK test
	row = db.QueryRow(`SELECT content FROM test_fts WHERE test_fts MATCH ?`, "心跳")
	var cn string
	if err := row.Scan(&cn); err != nil {
		t.Logf("CJK MATCH (expected to maybe fail with default tokenizer): %v", err)
	} else {
		t.Logf("CJK MATCH OK: %q", cn)
	}
}
