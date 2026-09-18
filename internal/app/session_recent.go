package app

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// ── `weiran session recent` — filesystem-backed historical session browser ──
//
// `session list` goes through the server and only knows about *running*
// sessions. Once a session exits, its JSONL sits in ~/.claude/projects/ or
// <appDir>/codex/ and becomes invisible to `list`.
//
// `session recent` fills that gap: walk all known JSONL locations, sort by
// mtime desc, optionally grep file contents for a keyword, and print a
// `session list`-style table. Local-only — does not require a running server.

type recentEntry struct {
	sid   string
	path  string
	mtime time.Time
	size  int64
}

// sessionRecent parses `recent` args and prints a table of recent JSONLs.
// Usage: weiran session recent [--grep KEYWORD] [-n N] [--json]
func sessionRecent(args []string) {
	limit := 20
	grep := ""
	asJSON := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-n":
			if i+1 < len(args) {
				if v, err := strconv.Atoi(args[i+1]); err == nil && v > 0 {
					limit = v
				}
				i++
			}
		case strings.HasPrefix(a, "-n="):
			if v, err := strconv.Atoi(strings.TrimPrefix(a, "-n=")); err == nil && v > 0 {
				limit = v
			}
		case a == "--grep" || a == "-g":
			if i+1 < len(args) {
				grep = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--grep="):
			grep = strings.TrimPrefix(a, "--grep=")
		case a == "--json":
			asJSON = true
		case a == "-h" || a == "--help":
			printRecentHelp()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", a)
			printRecentHelp()
			os.Exit(1)
		}
	}

	entries := collectRecentEntries()

	// Grep filter — content match (case-insensitive). Applied before sort so
	// the result count reflects the filter, not the pre-filter universe.
	if grep != "" {
		needle := strings.ToLower(grep)
		filtered := entries[:0]
		for _, e := range entries {
			if fileContainsCI(e.path, needle) {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].mtime.After(entries[j].mtime)
	})

	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}

	if asJSON {
		printRecentJSON(entries)
		return
	}

	if len(entries) == 0 {
		if grep != "" {
			fmt.Fprintf(os.Stderr, "no sessions matched %q\n", grep)
		} else {
			fmt.Fprintln(os.Stderr, "no sessions found")
		}
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SID\tMTIME\tSIZE\tPATH")
	for _, e := range entries {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			shortID(e.sid),
			e.mtime.Local().Format("2006-01-02 15:04"),
			humanBytes(e.size),
			abbrevHome(e.path),
		)
	}
	w.Flush()
}

func printRecentHelp() {
	fmt.Fprintf(os.Stderr, "usage: %s session recent [--grep KEYWORD] [-n N] [--json]\n", appName)
	fmt.Fprintln(os.Stderr, "  List recent session JSONL files by mtime across:")
	fmt.Fprintln(os.Stderr, "    - $CLAUDE_CONFIG_DIR/projects/*/*.jsonl")
	fmt.Fprintln(os.Stderr, "    - <appDir>/codex/*.jsonl")
	fmt.Fprintln(os.Stderr, "    - archive sources registered with `db add-source`")
	fmt.Fprintln(os.Stderr, "  Flags:")
	fmt.Fprintln(os.Stderr, "    -n N          limit output to N rows (default 20)")
	fmt.Fprintln(os.Stderr, "    --grep WORD   only show files whose content contains WORD (case-insensitive)")
	fmt.Fprintln(os.Stderr, "    --json        emit JSON instead of a table")
}

// collectRecentEntries walks every known JSONL location and returns a
// deduplicated list. Dedup is by absolute path since the same file can be
// reachable through multiple roots (e.g. symlinked archive sources).
func collectRecentEntries() []recentEntry {
	seen := map[string]bool{}
	var out []recentEntry

	add := func(p string) {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if seen[abs] {
			return
		}
		seen[abs] = true
		fi, err := os.Stat(abs)
		if err != nil {
			return
		}
		sid := strings.TrimSuffix(filepath.Base(abs), ".jsonl")
		out = append(out, recentEntry{
			sid:   sid,
			path:  abs,
			mtime: fi.ModTime(),
			size:  fi.Size(),
		})
	}

	walk := func(root string) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return
		}
		for _, e := range entries {
			full := filepath.Join(root, e.Name())
			if e.IsDir() {
				if sub, err := os.ReadDir(full); err == nil {
					for _, se := range sub {
						if !se.IsDir() && strings.HasSuffix(se.Name(), ".jsonl") {
							add(filepath.Join(full, se.Name()))
						}
					}
				}
				continue
			}
			if strings.HasSuffix(e.Name(), ".jsonl") {
				add(full)
			}
		}
	}

	// Claude Code projects
	walk(filepath.Join(claudeConfigDir, "projects"))

	// OpenClaw agent sessions
	if entries, err := os.ReadDir(filepath.Join(appHome, "agents")); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sessDir := filepath.Join(appHome, "agents", e.Name(), "sessions")
			if sub, err := os.ReadDir(sessDir); err == nil {
				for _, se := range sub {
					if !se.IsDir() && strings.HasSuffix(se.Name(), ".jsonl") {
						add(filepath.Join(sessDir, se.Name()))
					}
				}
			}
		}
	}

	// Registered archive sources (db add-source)
	for _, archProjects := range archiveProjectsDirs() {
		walk(archProjects)
	}

	return out
}

// fileContainsCI streams a file line-by-line checking for a case-insensitive
// substring match. Avoids loading huge JSONLs into memory.
func fileContainsCI(path, needle string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	// JSONL lines can be large — bump the max token size.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		if strings.Contains(strings.ToLower(scanner.Text()), needle) {
			return true
		}
	}
	return false
}

// humanBytes formats a byte count into a short human-readable string.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	suffix := "KMGTPE"[exp]
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), suffix)
}

// abbrevHome replaces the user's home dir with "~" for shorter paths.
func abbrevHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// printRecentJSON emits a machine-readable list of entries.
func printRecentJSON(entries []recentEntry) {
	var sb strings.Builder
	sb.WriteString("[")
	for i, e := range entries {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"sid":%q,"path":%q,"mtime":%q,"size":%d}`,
			e.sid, e.path, e.mtime.UTC().Format(time.RFC3339), e.size)
	}
	sb.WriteString("]\n")
	os.Stdout.WriteString(sb.String())
}
