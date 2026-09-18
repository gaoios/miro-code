package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── Session Search ──

type sessionInfo struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	Name      string    `json:"name"`
	Title     string    `json:"title"`
	FirstMsg  string    `json:"first_msg"`
	Model     string    `json:"model"`
	Size      int64     `json:"size"`
	Messages  int       `json:"messages"`
	ModTime   time.Time `json:"mod_time"`
	StartTime time.Time `json:"start_time"`
	Path      string    `json:"path"`
	Summary   string    `json:"summary,omitempty"`
}

func handleSessions(args []string) {
	// parse flags
	query := ""
	limit := 0
	showJSON := false
	showList := false
	projectFilter := ""
	daysFilter := 0
	var passthrough []string // flags forwarded to claude after resume (e.g. --chrome)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--chrome":
			passthrough = append(passthrough, "--chrome")
			continue
		case "-n":
			if i+1 < len(args) {
				limit, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "-j", "--json":
			showJSON = true
		case "-l", "--list":
			showList = true
		case "-d", "--days":
			if i+1 < len(args) {
				daysFilter, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "-P", "--project":
			if i+1 < len(args) {
				projectFilter = args[i+1]
				i++
			}
		case "-h", "--help":
			fmt.Print(`` + appName + ` sessions — Session Search

Usage:
  ` + appName + ` sessions [keyword]     interactive TUI (default)
  ` + appName + ` ss [keyword]           alias

Options:
  -l, --list     table output (non-interactive)
  -n <count>     number of entries (--list mode default 20)
  -d <days>      last N days only
  -P <project>   filter by project (fuzzy match)
  -j, --json     JSON output
  --chrome       launch resumed session with --chrome (browser tools)
  -h, --help     help

TUI shortcuts:
  ↑↓ / j k       move up/down
  / (slash)      search filter (live)
  enter          open selected session (claude -r <id>)
  pgup/pgdn      page up/down
  g / G          jump to top/bottom
  q / esc        quit

Examples:
  ` + appName + ` ss                    interactive browse all sessions
  ` + appName + ` ss nginx              pre-filter nginx related sessions
  ` + appName + ` ss -l                 table output last 20
  ` + appName + ` ss -l -d 3            table output last 3 days
  ` + appName + ` ss -P gallery -j      JSON output gallery project
`)
			return
		default:
			if query == "" {
				query = args[i]
			} else {
				query += " " + args[i]
			}
		}
	}

	sessions := scanAllSessions()

	// pre-filter by flags
	var filtered []sessionInfo
	queryLower := strings.ToLower(query)
	cutoff := time.Time{}
	if daysFilter > 0 {
		cutoff = time.Now().AddDate(0, 0, -daysFilter)
	}
	for _, s := range sessions {
		if !cutoff.IsZero() && s.ModTime.Before(cutoff) {
			continue
		}
		if projectFilter != "" && !strings.Contains(strings.ToLower(s.Project), strings.ToLower(projectFilter)) {
			continue
		}
		if (showList || showJSON) && query != "" && !sessionMatchesQuery(s, queryLower) {
			continue
		}
		filtered = append(filtered, s)
	}

	// sort by mod time desc
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].ModTime.After(filtered[j].ModTime)
	})

	// JSON output
	if showJSON {
		if limit > 0 && len(filtered) > limit {
			filtered = filtered[:limit]
		}
		out, _ := json.MarshalIndent(filtered, "", "  ")
		fmt.Println(string(out))
		return
	}

	// table output
	if showList {
		if limit == 0 {
			limit = 20
		}
		if limit > 0 && len(filtered) > limit {
			filtered = filtered[:limit]
		}
		printSessionTable(filtered, query)
		return
	}

	// TUI mode: pre-set search query if provided
	if query != "" {
		// pass query as initial filter in TUI
		chosen := runSessionsTUIWithQuery(filtered, query)
		if chosen != nil {
			execClaudeResume(chosen.ID, passthrough...)
		}
	} else {
		chosen := runSessionsTUI(filtered)
		if chosen != nil {
			execClaudeResume(chosen.ID, passthrough...)
		}
	}
}

func printSessionTable(filtered []sessionInfo, query string) {
	if len(filtered) == 0 {
		if query != "" {
			fmt.Printf("no matches found for \"%s\"\n", query)
		} else {
			fmt.Println("no sessions")
		}
		return
	}

	fmt.Printf("%-8s  %-19s  %-8s  %-4s  %-22s  %s\n", "ID", "Time", "Size", "Msgs", "Project", "Title")
	fmt.Println(strings.Repeat("─", 110))
	for _, s := range filtered {
		id := shortID(s.ID)
		t := s.ModTime.Format("2006-01-02 15:04:05")
		sz := humanSize(s.Size)
		proj := s.Project
		if len(proj) > 22 {
			proj = proj[:19] + "..."
		}
		desc := s.Title
		if desc == "" {
			desc = s.FirstMsg
		}
		if desc == "" && s.Summary != "" {
			desc = "[summary] " + s.Summary
		}
		if desc == "" {
			desc = s.Name
		}
		desc = strings.ReplaceAll(desc, "\n", " ")
		if len(desc) > 60 {
			desc = desc[:57] + "..."
		}
		msgs := fmt.Sprintf("%d", s.Messages)
		fmt.Printf("%-8s  %s  %8s  %4s  %-22s  %s\n", id, t, sz, msgs, proj, desc)
	}
	fmt.Printf("\n%d sessions total", len(filtered))
	if query != "" {
		fmt.Printf(" (matching \"%s\")", query)
	}
	fmt.Println()
}

func scanAllSessions() []sessionInfo {
	claudeProjects := filepath.Join(claudeConfigDir, "projects")
	var sessions []sessionInfo

	// load weiran DB summaries for enrichment
	summaryMap := loadSummaryMap()

	// load active session names from ~/.claude/sessions/
	nameMap := loadSessionNames()

	// scan active claude config dir
	scanProjectDir(claudeProjects, nameMap, summaryMap, &sessions, false)

	// scan archive sources
	for _, archiveDir := range archiveProjectsDirs() {
		scanProjectDir(archiveDir, nil, summaryMap, &sessions, true)
	}

	return sessions
}

// scanProjectDir reads a projects/ directory (like ~/.claude/projects/) and
// appends sessionInfo entries. When isArchive is true, session names are skipped
// (archives don't have active session metadata).
func scanProjectDir(projectsDir string, nameMap, summaryMap map[string]string, sessions *[]sessionInfo, isArchive bool) {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return
	}

	for _, projEntry := range entries {
		if !projEntry.IsDir() {
			continue
		}
		projName := decodeProjectName(projEntry.Name())
		projDir := filepath.Join(projectsDir, projEntry.Name())

		files, err := os.ReadDir(projDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			sessionID := strings.TrimSuffix(f.Name(), ".jsonl")
			fpath := filepath.Join(projDir, f.Name())
			info, err := f.Info()
			if err != nil {
				continue
			}

			s := sessionInfo{
				ID:      sessionID,
				Project: projName,
				Size:    info.Size(),
				ModTime: info.ModTime(),
				Path:    fpath,
			}

			// name from active sessions (archives don't have these)
			if !isArchive {
				if n, ok := nameMap[sessionID]; ok {
					s.Name = n
				}
			}

			// summary from weiran DB
			if sum, ok := summaryMap[fpath]; ok {
				s.Summary = sum
			}

			// parse JSONL head for title, first message, model, message count
			parseSessionHead(&s)

			*sessions = append(*sessions, s)
		}
	}
}

func parseSessionHead(s *sessionInfo) {
	f, err := os.Open(s.Path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// JSONL lines can include large base64 images / HTML payloads; 64 MB max
	// keeps us from aborting mid-file when a single line is oversized.
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	msgCount := 0
	linesRead := 0
	bytesRead := int64(0)
	maxLines := 500 // only scan head for speed

	for scanner.Scan() && linesRead < maxLines {
		linesRead++
		line := scanner.Bytes()
		bytesRead += int64(len(line)) + 1 // +1 for newline

		var ev struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			Title     string `json:"title"`
			Timestamp string `json:"timestamp"`
			Model     string `json:"model"` // init message has model at top level
			Message   struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
				Model   string          `json:"model"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}

		switch ev.Type {
		case "system":
			// init message contains the model used to start the session
			if ev.Subtype == "init" && s.Model == "" && ev.Model != "" {
				s.Model = ev.Model
			}
		case "custom-title":
			if s.Title == "" {
				s.Title = ev.Title
			}
		case "user":
			msgCount++
			if s.FirstMsg == "" {
				s.FirstMsg = extractText(ev.Message.Content)
			}
			if s.StartTime.IsZero() && ev.Timestamp != "" {
				s.StartTime, _ = time.Parse(time.RFC3339Nano, ev.Timestamp)
			}
		case "assistant":
			msgCount++
			if s.Model == "" && ev.Message.Model != "" {
				s.Model = ev.Message.Model
			}
		}
	}

	// if we only read head, estimate total messages from actual bytes read
	if linesRead >= maxLines && s.Size > 0 && bytesRead > 0 {
		s.Messages = int(float64(msgCount) * float64(s.Size) / float64(bytesRead))
	} else {
		s.Messages = msgCount
	}
}

func extractText(raw json.RawMessage) string {
	// try as string
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if len(s) > 200 {
			s = s[:200]
		}
		return s
	}
	// try as array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				t := b.Text
				if len(t) > 200 {
					t = t[:200]
				}
				return t
			}
		}
	}
	return ""
}

func sessionMatchesQuery(s sessionInfo, q string) bool {
	fields := []string{
		strings.ToLower(s.Title),
		strings.ToLower(s.FirstMsg),
		strings.ToLower(s.Name),
		strings.ToLower(s.Project),
		strings.ToLower(s.Summary),
		strings.ToLower(s.ID),
		strings.ToLower(s.Model),
	}
	// support multi-word: all words must match somewhere
	words := strings.Fields(q)
	for _, w := range words {
		found := false
		for _, f := range fields {
			if strings.Contains(f, w) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func loadSummaryMap() map[string]string {
	m := make(map[string]string)
	db, err := openDB()
	if err != nil {
		return m
	}
	defer db.Close()
	rows, err := db.Query("SELECT path, summary FROM sessions WHERE summary != ''")
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var path, summary string
		rows.Scan(&path, &summary)
		m[path] = summary
	}
	return m
}

func loadSessionNames() map[string]string {
	m := make(map[string]string)
	sessDir := filepath.Join(claudeConfigDir, "sessions")
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		return m
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(sessDir, e.Name()))
		if err != nil {
			continue
		}
		var meta struct {
			SessionID string `json:"sessionId"`
			Name      string `json:"name"`
		}
		if json.Unmarshal(data, &meta) == nil && meta.Name != "" {
			m[meta.SessionID] = meta.Name
		}
	}
	return m
}

func decodeProjectName(encoded string) string {
	// -Users-alice--openclaw-workspace → ~/.openclaw/workspace
	// Claude Code encodes: / → - and leading dot gets doubled --
	s := encoded
	// replace all - with /
	s = strings.ReplaceAll(s, "-", "/")
	// restore double-dash → dot: // → /.
	s = strings.ReplaceAll(s, "//", "/.")
	homePrefix := home + "/"
	if strings.HasPrefix(s, homePrefix) {
		s = "~/" + s[len(homePrefix):]
	}
	return s
}

func humanSize(b int64) string {
	switch {
	case b >= 1024*1024:
		return fmt.Sprintf("%.1fM", float64(b)/1024/1024)
	case b >= 1024:
		return fmt.Sprintf("%.0fK", float64(b)/1024)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// ── Session Collection ──

type sessionFile struct {
	source string
	path   string
	mtime  time.Time
}

func recentSessions(limit int) []sessionFile {
	cutoff := time.Now().AddDate(0, 0, -2) // 48h

	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if info, err := os.Stat(filepath.Join(workspace, "memory", yesterday+".md")); err == nil {
		cutoff = info.ModTime()
	}

	var all []sessionFile

	// Claude Code sessions
	ccDir := filepath.Join(claudeConfigDir, "projects")
	if entries, err := os.ReadDir(ccDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(ccDir, e.Name())
			collect(&all, dir, "cc", cutoff)
		}
	}

	// Archive sources (old .claude backups)
	for _, archiveDir := range archiveProjectsDirs() {
		if entries, err := os.ReadDir(archiveDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				dir := filepath.Join(archiveDir, e.Name())
				collect(&all, dir, "archive", cutoff)
			}
		}
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].mtime.After(all[j].mtime)
	})

	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

// ownSessionMarkers are markers to identify weiran's own recall/cron/heartbeat sessions
var ownSessionMarkers = []string{
	"boot recall",
	"memory consolidation",
	"heartbeat patrol",
	"db pending",
	"db save",
}

func isOwnSession(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	// read first 4KB to check
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	content := string(buf[:n])
	for _, marker := range ownSessionMarkers {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}

func collect(out *[]sessionFile, dir, source string, cutoff time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			continue
		}
		fullPath := filepath.Join(dir, e.Name())
		// skip weiran's own sessions to avoid infinite loops
		if isOwnSession(fullPath) {
			continue
		}
		*out = append(*out, sessionFile{
			source: source,
			path:   fullPath,
			mtime:  info.ModTime(),
		})
	}
}

// legacy formatting (for heartbeat and other scenarios not using DB)
func formatSessionList(sessions []sessionFile) string {
	if len(sessions) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Recent conversation logs below (cc=Claude Code, oc-*=OpenClaw):\n")
	for _, s := range sessions {
		fmt.Fprintf(&b, "- %s:%s\n", s.source, s.path)
	}
	return b.String()
}
