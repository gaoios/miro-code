package app

import (
	"database/sql"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"
)

// auditDB is a persistent connection to sessions.db for memory audit writes.
var auditDB *sql.DB
var auditDBMu sync.Mutex

// openAuditDB returns a persistent DB handle for memory audit operations.
// Uses the same sessions.db as openDB() but keeps it open.
// Retries on each call if previous attempt failed (unlike sync.Once which
// would permanently swallow the error).
func openAuditDB() *sql.DB {
	auditDBMu.Lock()
	defer auditDBMu.Unlock()
	if auditDB != nil {
		return auditDB
	}
	db, err := openDB()
	if err != nil {
		log.Printf("[memory-audit] failed to open DB: %v", err)
		return nil
	}
	auditDB = db
	return auditDB
}

// memoryAuditEntry is one audited memory operation.
type memoryAuditEntry struct {
	Timestamp   time.Time `json:"timestamp"`
	SessionID   string    `json:"session_id"`
	SessionName string    `json:"session_name"`
	Operation   string    `json:"operation"`          // recall, search, store, list
	ToolName    string    `json:"tool_name"`           // Read, Grep, Write, Edit, Glob
	Path        string    `json:"path"`                // target file/dir path
	Query       string    `json:"query,omitempty"`     // Grep pattern if applicable
	LatencyMS   int64     `json:"latency_ms,omitempty"` // tool_use → tool_result time
	Hit         bool      `json:"hit"`                 // result was non-empty/non-error
	ResultSize  int       `json:"result_size"`          // tool_result content length
}

// pendingMemoryOp tracks a tool_use that targets memory, waiting for its tool_result.
type pendingMemoryOp struct {
	ToolUseID string
	Operation string
	ToolName  string
	Path      string
	Query     string
	StartTime time.Time
}

// isMemoryPath returns true if the path references the memory directory or MEMORY.md index.
func isMemoryPath(p string) bool {
	return strings.Contains(p, "/memory/") || strings.HasSuffix(p, "/MEMORY.md") ||
		strings.Contains(p, "memory/topics/") || strings.Contains(p, "memory/evolve/")
}

// classifyMemoryToolUse checks if a tool_use targets memory and returns the operation details.
// Returns ok=false if this is not a memory operation.
func classifyMemoryToolUse(name string, input json.RawMessage) (op, path, query string, ok bool) {
	switch name {
	case "Read":
		var p struct {
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal(input, &p) == nil && isMemoryPath(p.FilePath) {
			return "recall", p.FilePath, "", true
		}
	case "Grep":
		var p struct {
			Path    string `json:"path"`
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal(input, &p) == nil && isMemoryPath(p.Path) {
			return "search", p.Path, p.Pattern, true
		}
	case "Write":
		var p struct {
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal(input, &p) == nil && isMemoryPath(p.FilePath) {
			return "store", p.FilePath, "", true
		}
	case "Edit":
		var p struct {
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal(input, &p) == nil && isMemoryPath(p.FilePath) {
			return "store", p.FilePath, "", true
		}
	case "Glob":
		var p struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if json.Unmarshal(input, &p) == nil && (isMemoryPath(p.Path) || isMemoryPath(p.Pattern)) {
			return "list", p.Path, p.Pattern, true
		}
	}
	return "", "", "", false
}

// extractMemoryToolUse parses a tool_use SSE message and returns a pendingMemoryOp
// if any content block targets memory. Returns nil otherwise.
func extractMemoryToolUse(raw json.RawMessage) *pendingMemoryOp {
	var peek struct {
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &peek) != nil {
		return nil
	}
	for _, c := range peek.Message.Content {
		if c.Type != "tool_use" {
			continue
		}
		op, path, query, ok := classifyMemoryToolUse(c.Name, c.Input)
		if !ok {
			continue
		}
		return &pendingMemoryOp{
			ToolUseID: c.ID,
			Operation: op,
			ToolName:  c.Name,
			Path:      path,
			Query:     query,
			StartTime: time.Now(),
		}
	}
	return nil
}

// matchMemoryToolResult checks if a tool_result message corresponds to a pending memory op.
// If matched, it removes the pending entry and returns a completed audit entry.
func matchMemoryToolResult(raw json.RawMessage, pending map[string]*pendingMemoryOp) *memoryAuditEntry {
	// tool_result messages are "user" type with content blocks referencing tool_use_id
	var peek struct {
		Message struct {
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
				Content   string `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &peek) != nil {
		return nil
	}
	for _, c := range peek.Message.Content {
		if c.Type != "tool_result" {
			continue
		}
		p, ok := pending[c.ToolUseID]
		if !ok {
			continue
		}
		delete(pending, c.ToolUseID)

		resultSize := len(c.Content)
		hit := resultSize > 0 && !strings.Contains(c.Content, "does not exist") &&
			!strings.Contains(c.Content, "Error:")

		return &memoryAuditEntry{
			Timestamp:  time.Now(),
			Operation:  p.Operation,
			ToolName:   p.ToolName,
			Path:       p.Path,
			Query:      p.Query,
			LatencyMS:  time.Since(p.StartTime).Milliseconds(),
			Hit:        hit,
			ResultSize: resultSize,
		}
	}
	return nil
}

// logMemoryAudit writes an audit entry to SQLite. Safe to call from a goroutine.
func logMemoryAudit(db *sql.DB, entry memoryAuditEntry) {
	if db == nil {
		return
	}
	_, err := db.Exec(`INSERT INTO memory_audit
		(timestamp, session_id, session_name, operation, tool_name, path, query, latency_ms, hit, result_size)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.Timestamp.Format(time.RFC3339),
		entry.SessionID, entry.SessionName,
		entry.Operation, entry.ToolName,
		entry.Path, entry.Query,
		entry.LatencyMS, entry.Hit, entry.ResultSize,
	)
	if err != nil {
		log.Printf("[memory-audit] write error: %v", err)
	}
}

// queryMemoryAudit retrieves audit entries within the given number of days.
func queryMemoryAudit(db *sql.DB, days int, op string, limit int) ([]memoryAuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	since := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)

	query := `SELECT timestamp, session_id, session_name, operation, tool_name,
		path, query, latency_ms, hit, result_size
		FROM memory_audit WHERE timestamp >= ?`
	args := []interface{}{since}

	if op != "" {
		query += " AND operation = ?"
		args = append(args, op)
	}
	query += " ORDER BY timestamp DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []memoryAuditEntry
	for rows.Next() {
		var e memoryAuditEntry
		var ts string
		if err := rows.Scan(&ts, &e.SessionID, &e.SessionName, &e.Operation,
			&e.ToolName, &e.Path, &e.Query, &e.LatencyMS, &e.Hit, &e.ResultSize); err != nil {
			continue
		}
		e.Timestamp, _ = time.Parse(time.RFC3339, ts)
		entries = append(entries, e)
	}
	return entries, nil
}

// memoryAuditStats holds aggregated memory audit statistics.
type memoryAuditStats struct {
	TotalOps    int                       `json:"total_ops"`
	ByOperation map[string]opStats        `json:"by_operation"`
	ByDay       []dayStats                `json:"by_day"`
	TopPaths    []pathStats               `json:"top_paths"`
}

type opStats struct {
	Count      int     `json:"count"`
	Hits       int     `json:"hits"`
	HitRate    float64 `json:"hit_rate"`
	AvgLatency int64   `json:"avg_latency_ms"`
}

type dayStats struct {
	Date    string  `json:"date"`
	Ops     int     `json:"ops"`
	Hits    int     `json:"hits"`
	HitRate float64 `json:"hit_rate"`
}

type pathStats struct {
	Path  string `json:"path"`
	Count int    `json:"count"`
}

// queryMemoryAuditStats returns aggregated statistics for the given number of days.
func queryMemoryAuditStats(db *sql.DB, days int) (*memoryAuditStats, error) {
	since := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)
	stats := &memoryAuditStats{
		ByOperation: make(map[string]opStats),
	}

	// By operation
	rows, err := db.Query(`SELECT operation, COUNT(*), SUM(hit), ROUND(AVG(latency_ms))
		FROM memory_audit WHERE timestamp >= ? GROUP BY operation`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var op string
		var s opStats
		if err := rows.Scan(&op, &s.Count, &s.Hits, &s.AvgLatency); err != nil {
			continue
		}
		if s.Count > 0 {
			s.HitRate = float64(s.Hits) / float64(s.Count)
		}
		stats.TotalOps += s.Count
		stats.ByOperation[op] = s
	}
	rows.Close()

	// By day
	rows, err = db.Query(`SELECT DATE(timestamp), COUNT(*), SUM(hit)
		FROM memory_audit WHERE timestamp >= ? GROUP BY DATE(timestamp) ORDER BY DATE(timestamp) DESC`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d dayStats
		if err := rows.Scan(&d.Date, &d.Ops, &d.Hits); err != nil {
			continue
		}
		if d.Ops > 0 {
			d.HitRate = float64(d.Hits) / float64(d.Ops)
		}
		stats.ByDay = append(stats.ByDay, d)
	}
	rows.Close()

	// Top paths
	rows, err = db.Query(`SELECT path, COUNT(*) as cnt
		FROM memory_audit WHERE timestamp >= ? GROUP BY path ORDER BY cnt DESC LIMIT 20`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p pathStats
		if err := rows.Scan(&p.Path, &p.Count); err != nil {
			continue
		}
		stats.TopPaths = append(stats.TopPaths, p)
	}
	rows.Close()

	return stats, nil
}
