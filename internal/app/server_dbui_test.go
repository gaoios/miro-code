package app

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardSelectOnly_Allows(t *testing.T) {
	cases := []string{
		"SELECT 1",
		"select * from sessions",
		"SELECT * FROM sessions LIMIT 50",
		"SELECT * FROM sessions LIMIT 50;",
		"  \n\tSELECT name FROM sqlite_master  ",
		"WITH t AS (SELECT 1 AS x) SELECT * FROM t",
		"with recursive c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i<5) SELECT * FROM c",
		"SELECT 'DROP TABLE users' AS harmless", // forbidden words inside string literal are OK
		"SELECT * FROM \"patterns\"",            // quoted identifier
		"/* header */ SELECT 1",
		"-- comment\nSELECT 2",
		"SELECT 'it''s fine' AS s",
	}
	for _, c := range cases {
		if err := guardSelectOnly(c); err != nil {
			t.Errorf("guardSelectOnly(%q) rejected, want allowed: %v", c, err)
		}
	}
}

func TestGuardSelectOnly_Rejects(t *testing.T) {
	// Any non-nil error is acceptable — detail varies (prefix / forbidden / multi).
	rejectAny := []string{
		"",
		"   ",
		";",
		"INSERT INTO sessions(path) VALUES('x')",
		"UPDATE sessions SET size=0",
		"DELETE FROM sessions",
		"DROP TABLE sessions",
		"CREATE TABLE t(a INT)",
		"ALTER TABLE sessions ADD COLUMN x TEXT",
		"TRUNCATE sessions",
		"ATTACH DATABASE 'x.db' AS x",
		"DETACH DATABASE x",
		"PRAGMA journal_mode=WAL",
		"VACUUM",
		"REINDEX",
		"BEGIN; SELECT 1; COMMIT",
		"SELECT 1; SELECT 2",
		"SELECT 1; DROP TABLE x",
		"EXPLAIN QUERY PLAN SELECT * FROM sessions",
		"REPLACE INTO sessions VALUES(1)",
	}
	for _, s := range rejectAny {
		if err := guardSelectOnly(s); err == nil {
			t.Errorf("guardSelectOnly(%q) allowed, want rejected", s)
		}
	}

	// Specific paths — verify the error message points at the right reason
	// so regressions that collapse two branches together show up.
	specific := []struct {
		sql, wantMatch string
	}{
		{"", "empty"},
		{"SELECT 1; SELECT 2", "multiple"},
		{"EXPLAIN QUERY PLAN SELECT * FROM sessions", "only select"},
		// Forbidden keyword *inside* an otherwise-SELECT statement — exercises
		// the forbidden-keyword branch (not the prefix check).
		{"SELECT * FROM (DROP TABLE t)", "forbidden"},
		{"WITH x AS (SELECT 1) SELECT * FROM x; DELETE FROM y", "multiple"},
	}
	for _, c := range specific {
		err := guardSelectOnly(c.sql)
		if err == nil {
			t.Errorf("guardSelectOnly(%q) allowed, want rejected", c.sql)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), c.wantMatch) {
			t.Errorf("guardSelectOnly(%q) err=%q, want containing %q", c.sql, err.Error(), c.wantMatch)
		}
	}
}

func TestValidIdentifier(t *testing.T) {
	good := []string{"sessions", "memory_audit", "t", "_hidden", "Tbl_123"}
	bad := []string{"", "1abc", "a-b", "sessions; DROP", "spaces ok", strings.Repeat("a", 200)}
	for _, s := range good {
		if !validIdentifier(s) {
			t.Errorf("validIdentifier(%q)=false, want true", s)
		}
	}
	for _, s := range bad {
		if validIdentifier(s) {
			t.Errorf("validIdentifier(%q)=true, want false", s)
		}
	}
}

func TestStripSQLCommentsAndStrings(t *testing.T) {
	// After stripping, forbidden words inside strings/comments should be gone.
	cases := []struct {
		in   string
		keep string // substring that should remain
		gone string // substring that should be stripped
	}{
		{"SELECT 'DROP TABLE'", "SELECT", "DROP"},
		{"SELECT /* DROP */ 1", "SELECT", "DROP"},
		{"SELECT 1 -- DROP TABLE\n", "SELECT", "DROP"},
		{"SELECT \"DROP\" FROM t", "SELECT", "DROP"},
	}
	for _, c := range cases {
		out := stripSQLCommentsAndStrings(c.in)
		if !strings.Contains(out, c.keep) {
			t.Errorf("strip(%q)=%q missing %q", c.in, out, c.keep)
		}
		if strings.Contains(out, c.gone) {
			t.Errorf("strip(%q)=%q still contains %q", c.in, out, c.gone)
		}
	}
}

func TestBuildDBRegistry_DefaultOnly(t *testing.T) {
	// With no configured sources, registry still exposes the primary `sessions`
	// source pointing at dbPath.
	origDB := dbPath
	dbPath = "/tmp/weiran-test/sessions.db"
	defer func() { dbPath = origDB }()

	reg := buildDBRegistry(dbExplorerConfig{})
	if reg.defaultName != "sessions" {
		t.Fatalf("default=%q, want sessions", reg.defaultName)
	}
	if len(reg.ordered) != 1 {
		t.Fatalf("ordered len=%d, want 1", len(reg.ordered))
	}
	if reg.ordered[0].Path != dbPath {
		t.Errorf("primary path=%q, want %q", reg.ordered[0].Path, dbPath)
	}
	if _, err := reg.resolveSource(""); err != nil {
		t.Errorf("resolveSource('') err=%v, want nil", err)
	}
	if _, err := reg.resolveSource("sessions"); err != nil {
		t.Errorf("resolveSource('sessions') err=%v, want nil", err)
	}
	if _, err := reg.resolveSource("nope"); err == nil {
		t.Errorf("resolveSource('nope') ok, want error")
	}
}

func TestBuildDBRegistry_CustomSources(t *testing.T) {
	origDB := dbPath
	dbPath = "/tmp/weiran-test/sessions.db"
	defer func() { dbPath = origDB }()

	cfg := dbExplorerConfig{
		Sources: []dbExplorerSource{
			{Name: "gallery", Path: "/var/tmp/gallery.db", Description: "gen gallery"},
			{Name: "proxy", Path: "~/proxy.db", Default: true},
			{Name: "bad-name", Path: "/x"},    // rejected: hyphen
			{Name: "", Path: "/x"},            // rejected: empty
			{Name: "nopath", Path: ""},        // rejected: empty path
		},
	}
	reg := buildDBRegistry(cfg)

	names := []string{}
	for _, s := range reg.ordered {
		names = append(names, s.Name)
	}
	want := []string{"sessions", "gallery", "proxy"}
	if len(names) != len(want) {
		t.Fatalf("ordered names=%v, want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("ordered[%d]=%q, want %q", i, names[i], n)
		}
	}

	if reg.defaultName != "proxy" {
		t.Errorf("defaultName=%q, want proxy", reg.defaultName)
	}
	// `~/proxy.db` must have been expanded.
	proxy, _ := reg.resolveSource("proxy")
	if strings.HasPrefix(proxy.Path, "~") {
		t.Errorf("proxy.Path=%q not expanded", proxy.Path)
	}
	if !filepath.IsAbs(proxy.Path) {
		t.Errorf("proxy.Path=%q not absolute", proxy.Path)
	}
}

func TestBuildDBRegistry_OverridePrimary(t *testing.T) {
	origDB := dbPath
	dbPath = "/tmp/weiran-test/sessions.db"
	defer func() { dbPath = origDB }()

	// User shadowing "sessions" with a different path: override in place,
	// don't duplicate.
	cfg := dbExplorerConfig{
		Sources: []dbExplorerSource{
			{Name: "sessions", Path: "/override/path/sessions.db"},
		},
	}
	reg := buildDBRegistry(cfg)
	if len(reg.ordered) != 1 {
		t.Fatalf("ordered len=%d, want 1 (override, not duplicate)", len(reg.ordered))
	}
	s, _ := reg.resolveSource("sessions")
	if s.Path != "/override/path/sessions.db" {
		t.Errorf("override didn't stick: path=%q", s.Path)
	}
}

func TestResolveHome(t *testing.T) {
	// "~/foo" expands; non-tilde passes through unchanged.
	out := resolveHome("~/foo/bar")
	if strings.HasPrefix(out, "~") {
		t.Errorf("resolveHome=%q still has ~", out)
	}
	if !strings.HasSuffix(out, "/foo/bar") {
		t.Errorf("resolveHome=%q missing suffix", out)
	}
	if resolveHome("/abs/path") != "/abs/path" {
		t.Error("resolveHome should not touch absolute path")
	}
	if resolveHome("") != "" {
		t.Error("resolveHome should preserve empty")
	}
}

func TestContainsWord(t *testing.T) {
	if !containsWord("SELECT * FROM DROP_TABLE_FOO", "DROP_TABLE_FOO") {
		t.Error("want match exact word")
	}
	if containsWord("SELECT dropt FROM t", "DROP") {
		t.Error("dropt should not match DROP as word")
	}
	if !containsWord("SELECT x; DROP TABLE y", "DROP") {
		t.Error("DROP with surrounding punctuation should match")
	}
}
