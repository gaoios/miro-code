package app

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// errSkipNonSession is returned for .jsonl files that don't contain session
// data (e.g. file-history-snapshot). Callers should treat it as "skip", not
// "fail" — these files are a normal part of a .claude directory.
var errSkipNonSession = errors.New("not a session file")

// migrateSessions implements `weiran db migrate-session`.
// Scrubs Anthropic API fingerprints from old session JSONLs and re-homes them
// under the active claudeConfigDir (or --to dir), so they can be searched /
// resumed under a different account without leaking the original account's
// requestId / msg_id identifiers.
//
// Fields handled:
//   - requestId              → deleted (API trace ID linked to original account)
//   - message.id             → deleted on assistant entries (Anthropic msg_xxx ID)
//   - sessionId              → regenerated (new UUID, new filename)
//   - uuid / parentUuid      → regenerated via remap (tree stays consistent)
//   - promptId               → regenerated via same remap
//   - cwd / gitBranch / etc. → preserved (per user spec, not identifying)
//   - project dir name       → preserved (same encoded form)
//
// Source can be a single .jsonl file or a directory — directories are walked
// recursively. Destination defaults to claudeConfigDir (respecting
// CLAUDE_CONFIG_DIR env var, which matters for alternate-bot setups like
// hengzhun that use their own config dir).
func migrateSessions(src, destDir string, dryRun, keepSource bool) (int, error) {
	if destDir == "" {
		destDir = claudeConfigDir
	}
	destProjects := filepath.Join(destDir, "projects")

	// Gather .jsonl files.
	// Source can be any of:
	//   - ~/.claude.bak                    (claude config root → walk projects/)
	//   - ~/.claude.bak/projects           (projects root       → walk subdirs)
	//   - ~/.claude.bak/projects/<proj>    (one project dir     → walk *.jsonl)
	//   - ~/.claude.bak/projects/<proj>/<sid>.jsonl (single file)
	// Only scans <root>/projects/*/* to avoid picking up non-session JSONLs
	// like history.jsonl or shell-snapshots.
	var files []string
	info, err := os.Stat(src)
	if err != nil {
		return 0, fmt.Errorf("source not found: %w", err)
	}
	if info.IsDir() {
		walkRoot := src
		// If <src>/projects exists, user passed the claude config root —
		// scan only projects/ to avoid non-session JSONLs.
		if fi, err := os.Stat(filepath.Join(src, "projects")); err == nil && fi.IsDir() {
			walkRoot = filepath.Join(src, "projects")
		}
		err = filepath.Walk(walkRoot, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if fi.IsDir() || !strings.HasSuffix(fi.Name(), ".jsonl") {
				return nil
			}
			// Require <...>/projects/<projDir>/<file>.jsonl layout, so the
			// parent-dir heuristic (used for the output project dir) holds.
			grand := filepath.Base(filepath.Dir(filepath.Dir(p)))
			if grand != "projects" {
				return nil
			}
			files = append(files, p)
			return nil
		})
		if err != nil {
			return 0, err
		}
	} else {
		if !strings.HasSuffix(src, ".jsonl") {
			return 0, fmt.Errorf("expected .jsonl file or directory")
		}
		files = []string{src}
	}
	if len(files) == 0 {
		return 0, fmt.Errorf("no session .jsonl files found under %s (expected <root>/projects/*/*.jsonl layout)", src)
	}

	var db *sql.DB
	if !dryRun {
		var err error
		db, err = openDB()
		if err != nil {
			return 0, fmt.Errorf("open db: %w", err)
		}
		defer db.Close()
	}

	migrated, skipped, failed := 0, 0, 0
	for _, f := range files {
		newPath, err := migrateSessionFile(f, destProjects, db, dryRun, keepSource)
		if errors.Is(err, errSkipNonSession) {
			skipped++
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "  FAIL %s: %v\n", filepath.Base(f), err)
			failed++
			continue
		}
		if newPath == "" {
			skipped++
			continue
		}
		verb := "migrated"
		if dryRun {
			verb = "would migrate"
		}
		fmt.Printf("  %s: %s → %s\n", verb, filepath.Base(f), newPath)
		migrated++
	}
	fmt.Printf("done: %d migrated, %d skipped (non-session), %d failed\n", migrated, skipped, failed)
	if failed > 0 {
		return migrated, fmt.Errorf("%d files failed", failed)
	}
	return migrated, nil
}

// migrateSessionFile processes a single JSONL file and writes the scrubbed
// version to destProjects/<origProjDirName>/<newSessionID>.jsonl.
// Returns the destination path on success, or "" if the file was empty/skipped.
func migrateSessionFile(srcPath, destProjects string, db *sql.DB, dryRun, keepSource bool) (string, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Read all lines first so we can build the UUID remap in one pass,
	// then rewrite in a second pass. Session JSONLs are typically <10MB,
	// so holding them in memory is fine.
	var rawLines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // up to 16MB per line
	for scanner.Scan() {
		rawLines = append(rawLines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan: %w", err)
	}
	if len(rawLines) == 0 {
		return "", nil
	}

	// Pass 1: parse every line, extract all UUIDs we need to remap.
	// Also capture first/last `timestamp` from the JSONL events so we can
	// restore the destination file's mtime to reflect the conversation's
	// actual last-activity time (not the migration time). Without this,
	// `session list --sort=mtime` and any downstream tools that rank by
	// file mtime see every migrated session as brand new.
	lines := make([]map[string]any, 0, len(rawLines))
	uuidRemap := map[string]string{}
	var oldSessionID string
	var firstTS, lastTS time.Time
	for _, ln := range rawLines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(ln), &obj); err != nil {
			// keep malformed lines out rather than corrupt the file
			continue
		}
		lines = append(lines, obj)
		if sid, ok := obj["sessionId"].(string); ok && oldSessionID == "" {
			oldSessionID = sid
		}
		if ts, ok := obj["timestamp"].(string); ok && ts != "" {
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				if firstTS.IsZero() || t.Before(firstTS) {
					firstTS = t
				}
				if t.After(lastTS) {
					lastTS = t
				}
			}
		}
		for _, k := range []string{"uuid", "parentUuid", "promptId"} {
			if v, ok := obj[k].(string); ok && v != "" {
				if _, exists := uuidRemap[v]; !exists {
					uuidRemap[v] = uuid.New().String()
				}
			}
		}
	}
	if oldSessionID == "" {
		// Not a conversation file — skip silently. Claude Code stores other
		// .jsonl files in projects/<dir>/ too (e.g. file-history-snapshot).
		return "", errSkipNonSession
	}
	newSessionID := uuid.New().String()

	// Destination: preserve the original project dir name (encodes cwd, which
	// we're not rewriting per spec). Source path looks like
	// <root>/projects/<projDir>/<oldSID>.jsonl, so parent dir name = projDir.
	projDir := filepath.Base(filepath.Dir(srcPath))
	destDir := filepath.Join(destProjects, projDir)
	destPath := filepath.Join(destDir, newSessionID+".jsonl")

	// Pass 2: scrub + rewrite.
	var out strings.Builder
	for _, obj := range lines {
		// Rewrite sessionId
		if _, ok := obj["sessionId"]; ok {
			obj["sessionId"] = newSessionID
		}
		// Remap UUID tree
		for _, k := range []string{"uuid", "parentUuid", "promptId"} {
			if v, ok := obj[k].(string); ok && v != "" {
				if nv, ok := uuidRemap[v]; ok {
					obj[k] = nv
				}
			}
		}
		// Strip API trace fields
		delete(obj, "requestId")
		if m, ok := obj["message"].(map[string]any); ok {
			delete(m, "id")
		}
		b, err := json.Marshal(obj)
		if err != nil {
			continue
		}
		out.Write(b)
		out.WriteByte('\n')
	}

	if dryRun {
		return destPath, nil
	}

	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", fmt.Errorf("mkdir dest: %w", err)
	}
	// Write atomically via temp file
	tmpPath := destPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(out.String()), 0644); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("rename: %w", err)
	}

	// Restore mtime/atime from the JSONL conversation timestamps so listings
	// sorted by file mtime reflect the session's actual last activity.
	// Fall back to doing nothing if the file had no parseable timestamps
	// (rare — unusual JSONL shape or all `timestamp` fields missing).
	if !lastTS.IsZero() {
		if err := os.Chtimes(destPath, lastTS, lastTS); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: could not set mtime for %s: %v\n", destPath, err)
		}
	}

	// Update weiran DB in full so the next index/recall pass sees the file as
	// unchanged: rewrite path, recompute hash/size from the new file, and
	// update session_content.sid to the new sessionId. Without this the next
	// scan would treat the file as changed and re-summarize / re-index.
	if db != nil {
		newHash, newSize := fileHash(destPath)
		var newMtime int64
		if !lastTS.IsZero() {
			// Prefer the conversation's last timestamp — that's the value we
			// just Chtimes'd on the file, so DB and disk agree.
			newMtime = lastTS.Unix()
		} else if fi, err := os.Stat(destPath); err == nil {
			newMtime = fi.ModTime().Unix()
		}

		if _, err := db.Exec(
			"UPDATE sessions SET path = ?, hash = ?, size = ? WHERE path = ?",
			destPath, newHash, newSize, srcPath); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: sessions DB update failed: %v\n", err)
		}
		if _, err := db.Exec(
			"UPDATE session_content SET path = ?, sid = ?, hash = ?, mtime = ? WHERE path = ?",
			destPath, newSessionID, newHash, newMtime, srcPath); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: session_content DB update failed: %v\n", err)
		}
		// Clean any stale row still pointing at the pre-move active path for
		// the same basename (e.g. session lived in ~/.claude before being
		// moved to ~/.claude.bak — that original row's file never existed
		// again and wouldn't get touched by gc's relocate due to PK conflict).
		baseNoExt := strings.TrimSuffix(filepath.Base(srcPath), ".jsonl")
		db.Exec(
			"DELETE FROM session_content WHERE sid = ? AND path != ?",
			baseNoExt, destPath)
		db.Exec(
			"DELETE FROM sessions WHERE path LIKE ? AND path != ?",
			"%/"+filepath.Base(srcPath), destPath)
	}

	// Delete source unless --keep-source: leaving the original around means
	// the next fts-index-sessions scan re-indexes it, re-creating the dup.
	if !keepSource {
		if err := os.Remove(srcPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "  warn: could not remove source %s: %v\n", srcPath, err)
		}
	}

	return destPath, nil
}
