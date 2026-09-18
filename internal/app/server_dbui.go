package app

// Simple read-only SQLite Explorer exposed under /api/db/*.
// Endpoints:
//   GET  /api/db/sources         — list configured DB sources
//   GET  /api/db/tables          — list user tables (name, type, ddl)
//   GET  /api/db/schema?table=x  — PRAGMA table_info + DDL for a single table
//   POST /api/db/query           — {"sql": "SELECT ..."} SELECT-only, timeout + row cap
//   GET  /api/db/ui              — minimal embedded HTML (textarea + Run + table)
//
// Multi-source: every data-plane endpoint accepts `?source=<name>` (tables/schema
// GET, query via JSON body or query param). Sources are whitelisted in config —
// clients cannot pass arbitrary paths.
//
// Safety: guardSelectOnly() rejects anything that isn't a single SELECT/WITH
// statement. Queries run inside a DEFERRED read-only transaction with a 5s
// context timeout and a 1000-row cap.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	dbUIQueryTimeout = 5 * time.Second
	dbUIRowCap       = 1000
	dbUIDefaultName  = "sessions" // always present, points at dbPath
)

// dbExplorerConfig is the on-disk config for /api/db/*. If the user doesn't
// configure it, we still expose the primary sessions.db under a default source
// so the UI "just works" out of the box.
type dbExplorerConfig struct {
	Enabled bool               `json:"enabled"` // default true (opt-out)
	Sources []dbExplorerSource `json:"sources"`

	enabledSet bool // true when json explicitly contained dbExplorer.enabled
}

type dbExplorerSource struct {
	Name        string `json:"name"`        // short identifier, e.g. "gallery"
	Path        string `json:"path"`        // absolute or ~/-relative path to .db
	Description string `json:"description"` // free text shown in UI
	Default     bool   `json:"default"`     // exactly one source may be default
	ReadOnly    bool   `json:"readOnly"`    // advisory flag (guard always enforces read-only)
}

// dbSourceRegistry holds the resolved, validated set of sources. Built once at
// server startup from config, then immutable.
type dbSourceRegistry struct {
	byName      map[string]dbExplorerSource
	ordered     []dbExplorerSource
	defaultName string
}

var (
	dbRegistry     *dbSourceRegistry
	dbRegistryOnce sync.Once
)

// resolveHome expands a leading "~/" or "~" to the user's home dir. Keeps all
// other paths untouched.
func resolveHome(p string) string {
	if p == "" {
		return p
	}
	if p == "~" {
		if u, err := user.Current(); err == nil {
			return u.HomeDir
		}
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if u, err := user.Current(); err == nil {
			return filepath.Join(u.HomeDir, p[2:])
		}
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

// buildDBRegistry resolves config into the final source set. Always installs the
// primary `sessions` source (pointing at dbPath) unless the user explicitly
// shadows it. Returns a registry with at least one entry.
func buildDBRegistry(cfg dbExplorerConfig) *dbSourceRegistry {
	reg := &dbSourceRegistry{byName: map[string]dbExplorerSource{}}

	// Default primary source — always available.
	primary := dbExplorerSource{
		Name:        dbUIDefaultName,
		Path:        dbPath,
		Description: "Primary session DB (" + filepath.Base(dbPath) + ")",
		Default:     true,
		ReadOnly:    true,
	}
	reg.byName[primary.Name] = primary
	reg.ordered = append(reg.ordered, primary)
	reg.defaultName = primary.Name

	seen := map[string]bool{primary.Name: true}

	for _, s := range cfg.Sources {
		name := strings.TrimSpace(s.Name)
		if !validIdentifier(name) {
			fmt.Fprintf(os.Stderr, "[%s] db-explorer: skipping source with invalid name %q\n", appName, s.Name)
			continue
		}
		path := resolveHome(strings.TrimSpace(s.Path))
		if path == "" {
			fmt.Fprintf(os.Stderr, "[%s] db-explorer: source %q has empty path, skipping\n", name, name)
			continue
		}
		abs, err := filepath.Abs(path)
		if err == nil {
			path = abs
		}
		src := dbExplorerSource{
			Name:        name,
			Path:        path,
			Description: strings.TrimSpace(s.Description),
			Default:     s.Default,
			ReadOnly:    true, // SELECT guard enforces read-only regardless
		}
		if seen[name] {
			// Replace primary / previous entry (user override).
			reg.byName[name] = src
			for i, x := range reg.ordered {
				if x.Name == name {
					reg.ordered[i] = src
					break
				}
			}
		} else {
			reg.byName[name] = src
			reg.ordered = append(reg.ordered, src)
			seen[name] = true
		}
		if s.Default {
			reg.defaultName = name
		}
	}
	return reg
}

// currentRegistry returns the process-wide registry, building it lazily from
// cfgForRegistry. Tests can reset this by assigning dbRegistry directly.
func currentRegistry() *dbSourceRegistry {
	dbRegistryOnce.Do(func() {
		if dbRegistry == nil {
			dbRegistry = buildDBRegistry(dbExplorerConfig{})
		}
	})
	return dbRegistry
}

func dbExplorerEnabled(cfg dbExplorerConfig) bool {
	if !cfg.enabledSet {
		return true
	}
	return cfg.Enabled
}

// resolveSource picks a source by name (or falls back to default when empty).
// Returns an error suitable for HTTP 400 if the name doesn't match.
func (reg *dbSourceRegistry) resolveSource(name string) (dbExplorerSource, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = reg.defaultName
	}
	s, ok := reg.byName[name]
	if !ok {
		return dbExplorerSource{}, fmt.Errorf("unknown source %q", name)
	}
	return s, nil
}

// openSourceDB opens a read-only connection to the given source. Callers must
// Close() the returned *sql.DB.
func openSourceDB(src dbExplorerSource) (*sql.DB, error) {
	// modernc.org/sqlite honours ?mode=ro in the DSN — gives us one more layer
	// of defense even if the guard is bypassed somehow.
	dsn := "file:" + src.Path + "?mode=ro&_txlock=deferred"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", src.Name, err)
	}
	db.SetMaxOpenConns(4)
	if _, err := db.ExecContext(context.Background(), "PRAGMA busy_timeout=3000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("pragma busy_timeout: %w", err)
	}
	return db, nil
}

// registerDBUIAPI wires the /api/db/* endpoints onto the given mux.
// Every endpoint is auth-protected via authMiddleware.
func registerDBUIAPI(mux *http.ServeMux, token string) {
	mux.HandleFunc("GET /api/db/sources", authMiddleware(token, handleDBSources))
	mux.HandleFunc("GET /api/db/tables", authMiddleware(token, handleDBTables))
	mux.HandleFunc("GET /api/db/schema", authMiddleware(token, handleDBSchema))
	mux.HandleFunc("POST /api/db/query", authMiddleware(token, handleDBQuery))
	// UI page itself is public (like GET /): browsers can't attach Bearer
	// headers on plain navigation. The HTML's JS reads the token from
	// localStorage (shared origin with the main UI) and uses it for API
	// calls. All data-plane endpoints above remain authed.
	mux.HandleFunc("GET /api/db/ui", handleDBUI)
}

// ── Handlers ──

func handleDBSources(w http.ResponseWriter, r *http.Request) {
	reg := currentRegistry()
	type out struct {
		Name        string `json:"name"`
		Path        string `json:"path"`
		Description string `json:"description,omitempty"`
		Default     bool   `json:"default,omitempty"`
		Exists      bool   `json:"exists"`
	}
	list := make([]out, 0, len(reg.ordered))
	for _, s := range reg.ordered {
		_, statErr := os.Stat(s.Path)
		list = append(list, out{
			Name:        s.Name,
			Path:        s.Path,
			Description: s.Description,
			Default:     s.Name == reg.defaultName,
			Exists:      statErr == nil,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sources": list,
		"default": reg.defaultName,
		"total":   len(list),
	})
}

func handleDBTables(w http.ResponseWriter, r *http.Request) {
	src, err := currentRegistry().resolveSource(r.URL.Query().Get("source"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	db, err := openSourceDB(src)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(r.Context(), dbUIQueryTimeout)
	defer cancel()

	rows, err := db.QueryContext(ctx,
		`SELECT name, type, COALESCE(sql, '') FROM sqlite_master
		   WHERE type IN ('table','view')
		     AND name NOT LIKE 'sqlite_%'
		   ORDER BY type, name`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	type tableInfo struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		DDL   string `json:"ddl"`
		Count *int64 `json:"count"` // nullable; null on timeout / error
	}
	var tables []tableInfo
	for rows.Next() {
		var t tableInfo
		if err := rows.Scan(&t.Name, &t.Type, &t.DDL); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Row counts — best-effort, per-table with short timeout. Counts that fail
	// (including slow sqlite_stat-less ones) are left nil so the UI can render
	// "—" gracefully instead of blocking the whole tree.
	skipCounts := r.URL.Query().Get("counts") == "0"
	if !skipCounts {
		for i := range tables {
			if !validIdentifier(tables[i].Name) {
				continue
			}
			cctx, ccancel := context.WithTimeout(r.Context(), 800*time.Millisecond)
			var n int64
			err := db.QueryRowContext(cctx, fmt.Sprintf(`SELECT COUNT(*) FROM %q`, tables[i].Name)).Scan(&n)
			ccancel()
			if err == nil {
				nn := n
				tables[i].Count = &nn
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"source": src.Name,
		"tables": tables,
		"total":  len(tables),
	})
}

func handleDBSchema(w http.ResponseWriter, r *http.Request) {
	src, err := currentRegistry().resolveSource(r.URL.Query().Get("source"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	table := strings.TrimSpace(r.URL.Query().Get("table"))
	if table == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "table required"})
		return
	}
	if !validIdentifier(table) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid table name"})
		return
	}

	db, err := openSourceDB(src)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(r.Context(), dbUIQueryTimeout)
	defer cancel()

	var ddl, kind string
	err = db.QueryRowContext(ctx,
		`SELECT COALESCE(sql,''), type FROM sqlite_master WHERE name = ?`, table).Scan(&ddl, &kind)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "table not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	colRows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%q)", table))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer colRows.Close()

	type colInfo struct {
		CID     int    `json:"cid"`
		Name    string `json:"name"`
		Type    string `json:"type"`
		NotNull int    `json:"notnull"`
		Default any    `json:"dflt_value"`
		PK      int    `json:"pk"`
	}
	var cols []colInfo
	for colRows.Next() {
		var c colInfo
		var dflt sql.NullString
		if err := colRows.Scan(&c.CID, &c.Name, &c.Type, &c.NotNull, &dflt, &c.PK); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if dflt.Valid {
			c.Default = dflt.String
		}
		cols = append(cols, c)
	}
	if err := colRows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"source":  src.Name,
		"table":   table,
		"type":    kind,
		"ddl":     ddl,
		"columns": cols,
	})
}

func handleDBQuery(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source string `json:"source"`
		SQL    string `json:"sql"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	// `?source=` in query wins over body only when body is empty, to keep URL
	// and body consistent (body is the canonical channel for POST).
	sourceName := body.Source
	if sourceName == "" {
		sourceName = r.URL.Query().Get("source")
	}
	src, err := currentRegistry().resolveSource(sourceName)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	sqlText := strings.TrimSpace(body.SQL)
	if sqlText == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sql required"})
		return
	}
	if err := guardSelectOnly(sqlText); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	db, err := openSourceDB(src)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(r.Context(), dbUIQueryTimeout)
	defer cancel()

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer tx.Rollback()

	start := time.Now()
	rows, err := tx.QueryContext(ctx, sqlText)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	resultRows := make([][]any, 0, 64)
	truncated := false
	for rows.Next() {
		if len(resultRows) >= dbUIRowCap {
			truncated = true
			break
		}
		scanDst := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range scanDst {
			ptrs[i] = &scanDst[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		for i, v := range scanDst {
			if b, ok := v.([]byte); ok {
				scanDst[i] = string(b)
			}
		}
		resultRows = append(resultRows, scanDst)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"source":     src.Name,
		"columns":    cols,
		"rows":       resultRows,
		"row_count":  len(resultRows),
		"truncated":  truncated,
		"row_cap":    dbUIRowCap,
		"elapsed_ms": time.Since(start).Milliseconds(),
	})
}

func handleDBUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(dbUIHTML))
}

// ── SELECT-only guard ──

// guardSelectOnly validates a SQL statement: must be a single SELECT or WITH
// statement, no forbidden keywords, no extra statements.
func guardSelectOnly(s string) error {
	stripped := stripSQLCommentsAndStrings(s)

	parts := strings.Split(stripped, ";")
	var stmts []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			stmts = append(stmts, strings.TrimSpace(p))
		}
	}
	if len(stmts) == 0 {
		return errors.New("empty statement")
	}
	if len(stmts) > 1 {
		return errors.New("multiple statements not allowed")
	}

	stmt := stmts[0]
	upper := strings.ToUpper(stmt)

	if !(hasWordPrefix(upper, "SELECT") || hasWordPrefix(upper, "WITH")) {
		return errors.New("only SELECT statements are allowed")
	}

	forbidden := []string{
		"INSERT", "UPDATE", "DELETE", "REPLACE",
		"DROP", "CREATE", "ALTER", "TRUNCATE",
		"ATTACH", "DETACH",
		"PRAGMA",
		"VACUUM", "REINDEX", "ANALYZE",
		"BEGIN", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE",
	}
	for _, kw := range forbidden {
		if containsWord(upper, kw) {
			return fmt.Errorf("forbidden keyword: %s", kw)
		}
	}
	return nil
}

// stripSQLCommentsAndStrings replaces single-quoted strings, double-quoted
// identifiers, `--` line comments, and `/* */` block comments with spaces.
func stripSQLCommentsAndStrings(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '-' && i+1 < len(s) && s[i+1] == '-' {
			for i < len(s) && s[i] != '\n' {
				b.WriteByte(' ')
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '*' {
			b.WriteString("  ")
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				b.WriteByte(' ')
				i++
			}
			if i+1 < len(s) {
				b.WriteString("  ")
				i += 2
			}
			continue
		}
		if c == '\'' {
			b.WriteByte(' ')
			i++
			for i < len(s) {
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						b.WriteString("  ")
						i += 2
						continue
					}
					b.WriteByte(' ')
					i++
					break
				}
				b.WriteByte(' ')
				i++
			}
			continue
		}
		if c == '"' {
			b.WriteByte(' ')
			i++
			for i < len(s) {
				if s[i] == '"' {
					if i+1 < len(s) && s[i+1] == '"' {
						b.WriteString("  ")
						i += 2
						continue
					}
					b.WriteByte(' ')
					i++
					break
				}
				b.WriteByte(' ')
				i++
			}
			continue
		}
		if c == '`' {
			b.WriteByte(' ')
			i++
			for i < len(s) && s[i] != '`' {
				b.WriteByte(' ')
				i++
			}
			if i < len(s) {
				b.WriteByte(' ')
				i++
			}
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func hasWordPrefix(s, word string) bool {
	if !strings.HasPrefix(s, word) {
		return false
	}
	if len(s) == len(word) {
		return true
	}
	return !isIdentChar(s[len(word)])
}

func containsWord(s, word string) bool {
	idx := 0
	for {
		p := strings.Index(s[idx:], word)
		if p < 0 {
			return false
		}
		start := idx + p
		end := start + len(word)
		leftOK := start == 0 || !isIdentChar(s[start-1])
		rightOK := end == len(s) || !isIdentChar(s[end])
		if leftOK && rightOK {
			return true
		}
		idx = end
	}
}

func isIdentChar(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}

// validIdentifier matches [A-Za-z_][A-Za-z0-9_]* up to 128 chars.
func validIdentifier(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if i == 0 && (b >= '0' && b <= '9') {
			return false
		}
		if !isIdentChar(b) {
			return false
		}
	}
	return true
}

// ── Embedded UI ──

const dbUIHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>DB Explorer</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
 :root {
   --bg:#0f1115; --panel:#161a22; --panel2:#1b2230; --border:#232a36;
   --text:#e6e6e6; --muted:#8a96a8; --muted2:#5f6b7d;
   --accent:#7aa2f7; --accent2:#bb9af7; --error:#f7768e; --ok:#9ece6a;
 }
 * { box-sizing: border-box; }
 body { margin:0; font:13px/1.4 -apple-system,BlinkMacSystemFont,"SF Mono",Menlo,monospace;
        background:var(--bg); color:var(--text); }
 header { padding:10px 14px; border-bottom:1px solid var(--border);
          display:flex; align-items:center; gap:12px; flex-wrap:wrap; }
 header h1 { margin:0; font-size:14px; font-weight:600; }
 header .muted { color:var(--muted); font-size:12px; }
 header select { background:var(--panel); color:var(--text);
                 border:1px solid var(--border); border-radius:4px;
                 padding:4px 8px; font:12px/1.2 inherit; }

 .layout { display:grid; grid-template-columns:260px 1fr; height:calc(100vh - 51px); }
 aside { border-right:1px solid var(--border); overflow:auto;
         display:flex; flex-direction:column; }
 .aside-h { padding:8px 10px 4px; font-size:11px; text-transform:uppercase;
            letter-spacing:.05em; color:var(--muted); display:flex;
            justify-content:space-between; align-items:center; }
 .src-info { font-size:11px; color:var(--muted); padding:0 10px 8px;
             word-break:break-all; }
 .src-info.missing { color:var(--error); }
 .tree { flex:1; overflow:auto; padding:0 4px 10px; }

 .t-row { display:flex; align-items:center; padding:3px 4px 3px 2px;
          border-radius:3px; cursor:pointer; user-select:none;
          white-space:nowrap; }
 .t-row:hover { background:var(--panel); }
 .t-row.active { background:var(--accent); color:#000; }
 .t-row.active .t-count, .t-row.active .t-type { color:#000; }
 .t-chevron { width:14px; text-align:center; color:var(--muted2);
              font-size:10px; flex:0 0 14px; }
 .t-chevron.none { visibility:hidden; }
 .t-name { flex:1; overflow:hidden; text-overflow:ellipsis; padding-right:6px; }
 .t-type { color:var(--muted2); font-size:10px; padding-right:4px; }
 .t-count { color:var(--muted); font-size:11px; font-variant-numeric:tabular-nums; }
 .t-cols { padding:1px 0 4px 22px; }
 .t-col { display:flex; padding:2px 4px; border-radius:3px; color:var(--muted);
          font-size:11px; cursor:default; }
 .t-col:hover { background:var(--panel); }
 .t-col-name { flex:1; overflow:hidden; text-overflow:ellipsis;
               color:var(--text); }
 .t-col-type { color:var(--muted2); padding-left:6px; font-size:10px; }
 .t-col-pk { color:var(--accent2); padding-right:4px; font-size:10px; }

 main { display:flex; flex-direction:column; overflow:hidden; }
 .tabs { display:flex; border-bottom:1px solid var(--border);
         background:var(--panel); }
 .tab { padding:8px 16px; cursor:pointer; font-size:12px; color:var(--muted);
        border-right:1px solid var(--border); user-select:none; }
 .tab.active { color:var(--text); background:var(--bg);
               border-bottom:2px solid var(--accent); margin-bottom:-1px; }
 .tab-title { margin-left:auto; padding:8px 14px; color:var(--muted);
              font-size:11px; }

 .pane { flex:1; display:none; flex-direction:column; overflow:hidden; }
 .pane.active { display:flex; }

 .toolbar { display:flex; gap:6px; align-items:center; padding:6px 10px;
            border-bottom:1px solid var(--border); background:var(--panel);
            font-size:11px; flex-wrap:wrap; }
 .toolbar .gap { flex:1; }
 .toolbar .status { color:var(--muted); }
 .toolbar .status.err { color:var(--error); }
 .toolbar .status.ok { color:var(--ok); }
 .toolbar input, .toolbar select {
   background:var(--panel2); color:var(--text); border:1px solid var(--border);
   border-radius:3px; padding:3px 6px; font:11px/1.2 inherit;
 }
 button { background:var(--accent); color:#000; border:0; border-radius:3px;
          padding:4px 10px; font-weight:600; cursor:pointer; font-size:11px; }
 button.secondary { background:var(--panel2); color:var(--text);
                    border:1px solid var(--border); font-weight:400; }
 button:disabled { opacity:.5; cursor:not-allowed; }

 .editor { padding:8px; display:flex; flex-direction:column; gap:6px;
           border-bottom:1px solid var(--border); }
 textarea { width:100%; min-height:130px; resize:vertical; background:var(--panel);
            color:var(--text); border:1px solid var(--border); border-radius:4px;
            padding:8px; font:12px/1.4 "SF Mono",Menlo,monospace; }

 .results { flex:1; overflow:auto; }
 table { border-collapse:collapse; width:100%; font-size:12px; }
 th, td { text-align:left; padding:4px 8px; border-bottom:1px solid var(--border);
          vertical-align:top; white-space:pre-wrap; word-break:break-word;
          max-width:520px; }
 th { position:sticky; top:0; background:var(--panel); font-weight:600;
      border-bottom:1px solid var(--border); cursor:pointer; user-select:none; }
 th .sort { color:var(--accent); font-size:10px; margin-left:4px; }
 tr:hover td { background:var(--panel); }
 .null { color:var(--muted2); font-style:italic; }

 pre.ddl { margin:0; padding:10px 14px; background:var(--panel);
           border-bottom:1px solid var(--border); font-size:12px;
           white-space:pre-wrap; color:var(--text); }
 .cols-table { width:100%; }
 .cols-table th { background:var(--panel); font-weight:600; }
</style>
</head>
<body>
<header>
  <h1>DB Explorer</h1>
  <label class="muted">source
    <select id="source"></select>
  </label>
  <span class="muted" id="hdr-note">read-only · SELECT / WITH · max 1000 rows · 5s timeout</span>
</header>
<div class="layout">
  <aside>
    <div class="aside-h">
      <span>Tables</span>
      <button class="secondary" id="refresh" title="Reload tables">↻</button>
    </div>
    <div class="src-info" id="srcInfo"></div>
    <div class="tree" id="tree"></div>
  </aside>

  <main>
    <div class="tabs">
      <div class="tab active" data-tab="data">Data</div>
      <div class="tab" data-tab="structure">Structure</div>
      <div class="tab" data-tab="query">Query</div>
      <div class="tab-title" id="tab-title"></div>
    </div>

    <!-- Data pane -->
    <div class="pane active" data-pane="data">
      <div class="toolbar" id="data-toolbar">
        <button class="secondary" id="page-first">⏮</button>
        <button class="secondary" id="page-prev">◀</button>
        <span id="page-info" class="status">—</span>
        <button class="secondary" id="page-next">▶</button>
        <button class="secondary" id="page-last">⏭</button>
        <label class="muted">rows
          <select id="page-size">
            <option>25</option><option selected>50</option>
            <option>100</option><option>250</option><option>500</option>
          </select>
        </label>
        <button class="secondary" id="data-refresh">Refresh</button>
        <div class="gap"></div>
        <span id="data-status" class="status"></span>
      </div>
      <div class="results" id="data-results">
        <p style="padding:20px;color:var(--muted)">Pick a table on the left.</p>
      </div>
    </div>

    <!-- Structure pane -->
    <div class="pane" data-pane="structure">
      <div class="toolbar">
        <span id="struct-title" class="muted">—</span>
        <div class="gap"></div>
      </div>
      <div class="results" id="struct-body">
        <p style="padding:20px;color:var(--muted)">Pick a table on the left.</p>
      </div>
    </div>

    <!-- Query pane -->
    <div class="pane" data-pane="query">
      <div class="editor">
        <textarea id="sql" placeholder="SELECT * FROM sessions LIMIT 50;"></textarea>
        <div class="toolbar" style="border:0;padding:0;background:transparent">
          <button id="run">Run (⌘/Ctrl+Enter)</button>
          <button id="clear" class="secondary">Clear</button>
          <div class="gap"></div>
          <span id="query-status" class="status"></span>
        </div>
      </div>
      <div class="results" id="query-results"></div>
    </div>
  </main>
</div>

<script>
(function(){
  var token = localStorage.getItem('weiran_token')
           || localStorage.getItem('soul_token')
           || new URLSearchParams(location.search).get('token')
           || '';
  if (!token) {
    var t = prompt('Auth token (saved to localStorage):');
    if (t) { localStorage.setItem('weiran_token', t); token = t; }
  }
  function headers(){ return { 'Authorization':'Bearer '+token, 'Content-Type':'application/json' }; }

  function $(id){ return document.getElementById(id); }
  function esc(s){ return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }
  function fmtNum(n){ return (n==null) ? '—' : String(n).replace(/\B(?=(\d{3})+(?!\d))/g,','); }

  var state = {
    source: null,
    sources: {},
    tables: [],             // [{name, type, ddl, count}]
    columnsCache: {},       // name -> [{cid,name,type,notnull,pk,...}]
    expanded: {},           // table -> bool (tree expansion)
    currentTable: null,
    tab: 'data',
    page: 0,
    pageSize: 50,
    sort: null,             // { col, dir:'asc'|'desc' }
    dataMeta: null,         // last fetched page meta {cols, rowCount, truncated, ...}
  };

  // ── tabs ──
  function switchTab(name){
    state.tab = name;
    document.querySelectorAll('.tab').forEach(function(el){
      el.classList.toggle('active', el.getAttribute('data-tab') === name);
    });
    document.querySelectorAll('.pane').forEach(function(el){
      el.classList.toggle('active', el.getAttribute('data-pane') === name);
    });
  }
  document.querySelectorAll('.tab').forEach(function(el){
    el.onclick = function(){ switchTab(el.getAttribute('data-tab')); };
  });

  function setStatus(id, msg, kind){
    var el = $(id);
    el.className = 'status' + (kind ? ' '+kind : '');
    el.textContent = msg || '';
  }

  function qsSource(){ return state.source ? ('source=' + encodeURIComponent(state.source)) : ''; }

  // ── sources ──
  function loadSources(){
    return fetch('/api/db/sources', { headers: headers() })
      .then(function(r){ return r.json(); })
      .then(function(d){
        if (d.error) { setStatus('data-status', d.error, 'err'); return; }
        renderSources(d.sources, d.default);
      });
  }
  function renderSources(list, def){
    var sel = $('source');
    sel.innerHTML = '';
    (list||[]).forEach(function(s){
      var opt = document.createElement('option');
      opt.value = s.name;
      opt.textContent = s.name + (s.exists ? '' : ' (missing)');
      opt.title = s.description || s.path;
      sel.appendChild(opt);
      state.sources[s.name] = s;
    });
    var saved = localStorage.getItem('weiran_db_source');
    var pick = (saved && state.sources[saved]) ? saved : def;
    if (pick && state.sources[pick]) sel.value = pick;
    state.source = sel.value;
    updateSrcInfo();
  }
  function updateSrcInfo(){
    var s = state.sources[state.source];
    var el = $('srcInfo');
    if (!s) { el.textContent = ''; el.className = 'src-info'; return; }
    el.textContent = (s.description ? s.description + ' · ' : '') + s.path;
    el.className = 'src-info' + (s.exists ? '' : ' missing');
  }

  // ── tree ──
  function loadTables(){
    $('tree').innerHTML = '<div style="padding:8px;color:var(--muted)">Loading…</div>';
    return fetch('/api/db/tables?' + qsSource(), { headers: headers() })
      .then(function(r){ return r.json(); })
      .then(function(d){
        if (d.error) { $('tree').innerHTML=''; setStatus('data-status', d.error, 'err'); return; }
        state.tables = d.tables || [];
        state.columnsCache = {};
        state.expanded = {};
        renderTree();
      })
      .catch(function(e){ setStatus('data-status', String(e), 'err'); });
  }

  function renderTree(){
    var root = $('tree');
    root.innerHTML = '';
    state.tables.forEach(function(t){
      var wrap = document.createElement('div');
      wrap.setAttribute('data-tbl', t.name);

      var row = document.createElement('div');
      row.className = 't-row';
      if (state.currentTable === t.name) row.classList.add('active');

      var chev = document.createElement('span');
      chev.className = 't-chevron';
      chev.textContent = state.expanded[t.name] ? '▼' : '▶';
      chev.onclick = function(ev){
        ev.stopPropagation();
        toggleExpand(t.name);
      };

      var name = document.createElement('span');
      name.className = 't-name';
      name.textContent = t.name;
      name.title = t.ddl || '';

      var type = document.createElement('span');
      type.className = 't-type';
      if (t.type === 'view') type.textContent = 'view';

      var count = document.createElement('span');
      count.className = 't-count';
      count.textContent = fmtNum(t.count);

      row.appendChild(chev);
      row.appendChild(name);
      row.appendChild(type);
      row.appendChild(count);

      row.onclick = function(){
        selectTable(t.name, true);
      };

      wrap.appendChild(row);

      if (state.expanded[t.name]) {
        var colsBox = document.createElement('div');
        colsBox.className = 't-cols';
        var cols = state.columnsCache[t.name];
        if (!cols) {
          colsBox.innerHTML = '<div class="t-col"><span class="t-col-name">Loading…</span></div>';
          fetchColumns(t.name).then(function(){ renderTree(); });
        } else {
          cols.forEach(function(c){
            var line = document.createElement('div');
            line.className = 't-col';
            var pk = document.createElement('span');
            pk.className = 't-col-pk';
            pk.textContent = c.pk ? '🔑' : '';
            var n = document.createElement('span');
            n.className = 't-col-name';
            n.textContent = c.name;
            var ty = document.createElement('span');
            ty.className = 't-col-type';
            ty.textContent = c.type || '';
            line.appendChild(pk);
            line.appendChild(n);
            line.appendChild(ty);
            colsBox.appendChild(line);
          });
        }
        wrap.appendChild(colsBox);
      }

      root.appendChild(wrap);
    });
  }

  function toggleExpand(name){
    state.expanded[name] = !state.expanded[name];
    if (state.expanded[name] && !state.columnsCache[name]) {
      fetchColumns(name).then(renderTree);
    } else {
      renderTree();
    }
  }

  function fetchColumns(table){
    return fetch('/api/db/schema?' + qsSource() + '&table=' + encodeURIComponent(table),
                 { headers: headers() })
      .then(function(r){ return r.json(); })
      .then(function(d){
        if (d.error) return;
        state.columnsCache[table] = d.columns || [];
        state.tables.forEach(function(t){
          if (t.name === table) t._ddl = d.ddl;
        });
      });
  }

  function selectTable(name, switchToData){
    state.currentTable = name;
    state.page = 0;
    state.sort = null;
    $('tab-title').textContent = name;
    renderTree();
    if (switchToData) switchTab('data');
    loadDataPage();
    renderStructure();
  }

  // ── Data pane ──
  function buildDataSQL(){
    var t = state.currentTable;
    if (!t) return '';
    var sql = 'SELECT * FROM "' + t.replace(/"/g, '""') + '"';
    if (state.sort) {
      sql += ' ORDER BY "' + state.sort.col.replace(/"/g, '""') + '" ' + state.sort.dir.toUpperCase();
    }
    sql += ' LIMIT ' + state.pageSize + ' OFFSET ' + (state.page * state.pageSize);
    return sql;
  }

  function loadDataPage(){
    if (!state.currentTable) { $('data-results').innerHTML = ''; return; }
    setStatus('data-status', 'loading…');
    var sql = buildDataSQL();
    return fetch('/api/db/query', {
      method:'POST', headers:headers(),
      body: JSON.stringify({ source: state.source, sql: sql }),
    })
      .then(function(r){ return r.json().then(function(d){ return {status:r.status,data:d}; }); })
      .then(function(res){
        if (res.status >= 400 || res.data.error) {
          $('data-results').innerHTML = '';
          setStatus('data-status', res.data.error || ('HTTP '+res.status), 'err');
          return;
        }
        state.dataMeta = res.data;
        renderDataTable(res.data);
        var t = state.tables.filter(function(x){ return x.name===state.currentTable; })[0];
        var total = t ? t.count : null;
        var start = state.page * state.pageSize + 1;
        var end = state.page * state.pageSize + res.data.row_count;
        setStatus('data-status',
          res.data.row_count + ' rows · ' + res.data.elapsed_ms + 'ms', 'ok');
        updatePageInfo(start, end, total);
      })
      .catch(function(e){ setStatus('data-status', String(e), 'err'); });
  }

  function renderDataTable(data){
    var cols = data.columns || [], rows = data.rows || [];
    if (!cols.length) {
      $('data-results').innerHTML = '<p style="padding:12px;color:var(--muted)">No columns.</p>';
      return;
    }
    var html = '<table><thead><tr>';
    cols.forEach(function(c){
      var arrow = '';
      if (state.sort && state.sort.col === c) {
        arrow = '<span class="sort">' + (state.sort.dir==='asc' ? '↑' : '↓') + '</span>';
      }
      html += '<th data-col="' + esc(c) + '">' + esc(c) + arrow + '</th>';
    });
    html += '</tr></thead><tbody>';
    rows.forEach(function(r){
      html += '<tr>';
      r.forEach(function(v){
        html += renderCell(v);
      });
      html += '</tr>';
    });
    html += '</tbody></table>';
    $('data-results').innerHTML = html;

    // Sort handler on column headers
    $('data-results').querySelectorAll('th').forEach(function(th){
      th.onclick = function(){
        var col = th.getAttribute('data-col');
        if (!state.sort || state.sort.col !== col) {
          state.sort = { col: col, dir: 'asc' };
        } else if (state.sort.dir === 'asc') {
          state.sort.dir = 'desc';
        } else {
          state.sort = null;
        }
        state.page = 0;
        loadDataPage();
      };
    });
  }

  function renderCell(v){
    if (v === null || v === undefined) return '<td><span class="null">NULL</span></td>';
    if (typeof v === 'object') return '<td>' + esc(JSON.stringify(v)) + '</td>';
    return '<td>' + esc(String(v)) + '</td>';
  }

  function updatePageInfo(start, end, total){
    var txt;
    if (end < start) txt = '0 of ' + fmtNum(total);
    else if (total != null) txt = fmtNum(start) + '–' + fmtNum(end) + ' of ' + fmtNum(total);
    else txt = fmtNum(start) + '–' + fmtNum(end);
    $('page-info').textContent = txt;

    $('page-prev').disabled = state.page === 0;
    $('page-first').disabled = state.page === 0;
    if (total != null) {
      var lastPage = Math.max(0, Math.ceil(total / state.pageSize) - 1);
      $('page-next').disabled = state.page >= lastPage;
      $('page-last').disabled = state.page >= lastPage;
    } else {
      // unknown total: disable "next" if we got fewer than pageSize
      var less = state.dataMeta && state.dataMeta.row_count < state.pageSize;
      $('page-next').disabled = !!less;
      $('page-last').disabled = true;
    }
  }

  $('page-first').onclick = function(){ state.page = 0; loadDataPage(); };
  $('page-prev').onclick  = function(){ if (state.page>0) { state.page--; loadDataPage(); } };
  $('page-next').onclick  = function(){ state.page++; loadDataPage(); };
  $('page-last').onclick  = function(){
    var t = state.tables.filter(function(x){ return x.name===state.currentTable; })[0];
    if (t && t.count != null) {
      state.page = Math.max(0, Math.ceil(t.count / state.pageSize) - 1);
      loadDataPage();
    }
  };
  $('page-size').onchange = function(){
    state.pageSize = parseInt($('page-size').value, 10) || 50;
    state.page = 0;
    loadDataPage();
  };
  $('data-refresh').onclick = loadDataPage;
  $('refresh').onclick = loadTables;

  // ── Structure pane ──
  function renderStructure(){
    var t = state.currentTable;
    if (!t) {
      $('struct-title').textContent = '—';
      $('struct-body').innerHTML = '<p style="padding:20px;color:var(--muted)">Pick a table.</p>';
      return;
    }
    $('struct-title').textContent = t;
    var tbl = state.tables.filter(function(x){ return x.name===t; })[0];
    var ddl = (tbl && (tbl._ddl || tbl.ddl)) || '';
    var cols = state.columnsCache[t];
    var html = '';
    if (ddl) html += '<pre class="ddl">' + esc(ddl) + '</pre>';
    if (cols) {
      html += '<table class="cols-table"><thead><tr>';
      html += '<th>#</th><th>name</th><th>type</th><th>null</th><th>default</th><th>pk</th>';
      html += '</tr></thead><tbody>';
      cols.forEach(function(c){
        html += '<tr>';
        html += '<td>' + c.cid + '</td>';
        html += '<td>' + esc(c.name) + '</td>';
        html += '<td>' + esc(c.type||'') + '</td>';
        html += '<td>' + (c.notnull ? 'NOT NULL' : '') + '</td>';
        html += '<td>' + (c.dflt_value==null ? '' : esc(String(c.dflt_value))) + '</td>';
        html += '<td>' + (c.pk ? '🔑' : '') + '</td>';
        html += '</tr>';
      });
      html += '</tbody></table>';
    } else {
      html += '<p style="padding:12px;color:var(--muted)">Loading columns…</p>';
      fetchColumns(t).then(renderStructure);
    }
    $('struct-body').innerHTML = html;
  }

  // ── Query pane ──
  function runQuery(){
    var sql = $('sql').value.trim();
    if (!sql) return;
    $('run').disabled = true;
    setStatus('query-status', 'running…');
    fetch('/api/db/query', {
      method:'POST', headers:headers(),
      body: JSON.stringify({ source: state.source, sql: sql }),
    })
      .then(function(r){ return r.json().then(function(d){ return {status:r.status,data:d}; }); })
      .then(function(res){
        $('run').disabled = false;
        if (res.status >= 400 || res.data.error) {
          $('query-results').innerHTML = '';
          setStatus('query-status', res.data.error || ('HTTP '+res.status), 'err');
          return;
        }
        var cols = res.data.columns || [], rows = res.data.rows || [];
        var html = '<table><thead><tr>';
        cols.forEach(function(c){ html += '<th>' + esc(c) + '</th>'; });
        html += '</tr></thead><tbody>';
        rows.forEach(function(r){
          html += '<tr>';
          r.forEach(function(v){ html += renderCell(v); });
          html += '</tr>';
        });
        html += '</tbody></table>';
        $('query-results').innerHTML = html;
        var msg = res.data.row_count + ' rows · ' + res.data.elapsed_ms + 'ms';
        if (res.data.truncated) msg += ' · truncated at ' + res.data.row_cap;
        setStatus('query-status', msg, 'ok');
      })
      .catch(function(e){ $('run').disabled=false; setStatus('query-status', String(e), 'err'); });
  }
  $('run').onclick = runQuery;
  $('clear').onclick = function(){ $('sql').value=''; $('query-results').innerHTML=''; setStatus('query-status',''); };
  $('sql').addEventListener('keydown', function(e){
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') { e.preventDefault(); runQuery(); }
  });

  // ── source change ──
  $('source').addEventListener('change', function(){
    state.source = $('source').value;
    localStorage.setItem('weiran_db_source', state.source);
    state.currentTable = null;
    state.page = 0;
    state.sort = null;
    $('tab-title').textContent = '';
    $('data-results').innerHTML = '<p style="padding:20px;color:var(--muted)">Pick a table on the left.</p>';
    $('struct-body').innerHTML = '<p style="padding:20px;color:var(--muted)">Pick a table on the left.</p>';
    updateSrcInfo();
    loadTables();
  });

  loadSources().then(loadTables);
})();
</script>
</body>
</html>
`
