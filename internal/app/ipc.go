package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// ── IPC CLI: weiran session list/send/read/search ──

// handleSessionIPC dispatches `weiran session <subcommand>` CLI commands.
func handleSessionIPC(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: %s session <list|recent|send|read|search|close|wait>\n", appName)
		os.Exit(1)
	}

	switch args[0] {
	case "list":
		ipcList()
	case "recent":
		sessionRecent(args[1:])
	case "send":
		if len(args) < 3 {
			fmt.Fprintf(os.Stderr, "usage: %s session send <target_id> \"message\"\n", appName)
			os.Exit(1)
		}
		msg := strings.Join(args[2:], " ")
		ipcSend(args[1], msg)
	case "read":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "usage: %s session read <target_id> [--verbose|-v] [--ts]\n", appName)
			os.Exit(1)
		}
		verbose := false
		showTS := false
		var targetID string
		for _, a := range args[1:] {
			switch a {
			case "--verbose", "-v":
				verbose = true
			case "--ts":
				showTS = true
			default:
				if targetID == "" {
					targetID = a
				}
			}
		}
		if targetID == "" {
			fmt.Fprintf(os.Stderr, "usage: %s session read <target_id> [--verbose|-v] [--ts]\n", appName)
			os.Exit(1)
		}
		ipcRead(targetID, verbose, showTS)
	case "search":
		if len(args) < 3 {
			fmt.Fprintf(os.Stderr, "usage: %s session search <target_id> \"keyword\"\n", appName)
			os.Exit(1)
		}
		keyword := strings.Join(args[2:], " ")
		ipcSearch(args[1], keyword)
	case "close":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "usage: %s session close <target_id>\n", appName)
			os.Exit(1)
		}
		ipcClose(args[1])
	case "wait":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "usage: %s session wait <target_id>\n", appName)
			os.Exit(1)
		}
		ipcWait(args[1])
	case "prepare-restart":
		// Optional: custom message as args[1:]
		msg := ""
		if len(args) > 1 {
			msg = strings.Join(args[1:], " ")
		}
		ipcPrepareRestart(msg)
	default:
		fmt.Fprintf(os.Stderr, "unknown session subcommand: %s\n", args[0])
		fmt.Fprintf(os.Stderr, "usage: %s session <list|recent|send|read|search|close|wait|prepare-restart>\n", appName)
		os.Exit(1)
	}
}

// ipcEnvPrefix returns the uppercase env var prefix derived from appName.
// e.g. "weiran" → "WEIRAN", "my-soul" → "MY_SOUL"
func ipcEnvPrefix() string {
	return strings.ToUpper(strings.ReplaceAll(appName, "-", "_"))
}

// ipcEnv reads IPC env vars and exits if missing.
func ipcEnv() (serverURL, token, sessionID string) {
	prefix := ipcEnvPrefix()
	serverURL = os.Getenv(prefix + "_SERVER_URL")
	token = os.Getenv(prefix + "_AUTH_TOKEN")
	sessionID = os.Getenv(prefix + "_SESSION_ID")

	if serverURL == "" || token == "" {
		fmt.Fprintf(os.Stderr, "error: %s_SERVER_URL and %s_AUTH_TOKEN must be set\n", prefix, prefix)
		fmt.Fprintf(os.Stderr, "  (these are automatically injected when running inside a server session)\n")
		os.Exit(1)
	}
	return
}

// ipcRequest makes an authenticated HTTP request to the server.
func ipcRequest(method, path string, body io.Reader) (*http.Response, error) {
	serverURL, token, _ := ipcEnv()
	u := strings.TrimRight(serverURL, "/") + path

	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

// ipcList lists active sessions + historical sessions from the last 24h.
// Shows ID, TURNS, CREATED, LAST_ACTIVE, NAME, STATUS, MODEL, CATEGORY.
func ipcList() {
	// 1. Fetch active sessions
	resp, err := ipcRequest("GET", "/api/sessions", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	var activeSessions []map[string]any
	json.NewDecoder(resp.Body).Decode(&activeSessions)
	resp.Body.Close()

	// 2. Fetch history (last 50, all categories) for 24h window
	resp2, err := ipcRequest("GET", "/api/history?limit=50&category=all", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	var historySessions []map[string]any
	json.NewDecoder(resp2.Body).Decode(&historySessions)
	resp2.Body.Close()

	// 3. Build dedup map: active sessions take priority
	_, _, myID := ipcEnv()
	seen := make(map[string]bool)    // track by weiran session ID
	seenCC := make(map[string]bool)  // track by claude_session_id
	var rows []map[string]any

	// Add active sessions first
	for _, s := range activeSessions {
		id := fmt.Sprintf("%v", s["id"])
		ccSID, _ := s["claude_session_id"].(string)
		seen[id] = true
		if ccSID != "" {
			seenCC[ccSID] = true
		}
		rows = append(rows, s)
	}

	// Add history sessions not already covered, filter to 24h
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, s := range historySessions {
		// History entries use claude_session_id as "id"
		ccSID := fmt.Sprintf("%v", s["id"])

		// Dedup by ccSID or by looking at weiran session ID
		if seenCC[ccSID] {
			continue
		}
		// Also check if there's a weiran SID in the data
		if weiranSID, ok := s["weiran_id"]; ok {
			wsID := fmt.Sprintf("%v", weiranSID)
			if seen[wsID] {
				continue
			}
		}

		// Filter to 24h by mod_time
		modTimeStr, _ := s["mod_time"].(string)
		if modTimeStr == "" {
			continue
		}
		modTime, err := time.Parse(time.RFC3339, modTimeStr)
		if err != nil || modTime.Before(cutoff) {
			continue
		}

		rows = append(rows, s)
	}

	// 4. Sort: active first (by last_active desc), then history (by mod_time desc)
	sort.Slice(rows, func(i, j int) bool {
		iActive := isSessionActive(rows[i])
		jActive := isSessionActive(rows[j])
		if iActive != jActive {
			return iActive // active before inactive
		}
		return getSessionTime(rows[i]).After(getSessionTime(rows[j]))
	})

	// 5. Render table
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tCC_ID\tTURNS\tCREATED\tLAST_ACTIVE\tNAME\tSTATUS\tMODEL\tCAT")
	for _, s := range rows {
		id := fmt.Sprintf("%v", s["id"])

		// Resolve weiran ID and Claude Code (cc) ID separately.
		// - Active sessions:  id = weiran ID, claude_session_id = cc ID
		// - History sessions: id = cc ID,     weiran_id (if present) = weiran ID
		var wID, ccID string
		if isSessionActive(s) {
			wID = id
			ccID = strVal(s, "claude_session_id")
		} else {
			ccID = id
			wID = strVal(s, "weiran_id")
		}
		// If active row also exposes a weiran_id, prefer the explicit field.
		if v := strVal(s, "weiran_id"); v != "" {
			wID = v
		}
		shortW := "-"
		if wID != "" {
			shortW = shortID(wID)
		}
		shortCC := "-"
		if ccID != "" {
			shortCC = shortID(ccID)
		}

		// Turns: num_turns (active) or user_turns (history) or messages count
		turns := "-"
		if v, ok := s["num_turns"]; ok && v != nil {
			turns = fmt.Sprintf("%v", v)
		} else if v, ok := s["user_turns"]; ok && v != nil {
			turns = fmt.Sprintf("%v", v)
		} else if v, ok := s["messages"]; ok && v != nil {
			n := 0
			switch val := v.(type) {
			case float64:
				n = int(val)
			case int:
				n = val
			}
			if n > 0 {
				turns = fmt.Sprintf("~%d", n)
			}
		}

		// Created time
		created := "-"
		if v, ok := s["created_at"].(string); ok && v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				created = t.Local().Format("01/02 15:04")
			}
		}

		// Last active time
		lastActive := "-"
		if v, ok := s["last_active"].(string); ok && v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				lastActive = t.Local().Format("15:04")
			}
		}
		// For history sessions, try updated_at then mod_time
		if lastActive == "-" {
			if v, ok := s["updated_at"].(string); ok && v != "" {
				if t, err := time.Parse(time.RFC3339, v); err == nil {
					lastActive = t.Local().Format("01/02 15:04")
				}
			}
		}
		if lastActive == "-" {
			if v, ok := s["mod_time"].(string); ok && v != "" {
				if t, err := time.Parse(time.RFC3339, v); err == nil {
					lastActive = t.Local().Format("01/02 15:04")
				}
			}
		}

		name := strVal(s, "name")
		if name == "" {
			name = strVal(s, "title")
		}
		if name == "" {
			// Use first_msg as fallback, truncated
			if fm := strVal(s, "first_msg"); fm != "" {
				if len(fm) > 30 {
					name = fm[:27] + "..."
				} else {
					name = fm
				}
			}
		}
		marker := ""
		if id == myID || wID == myID {
			marker = " (me)"
		}
		// Truncate long names
		if len(name) > 30 {
			name = name[:27] + "..."
		}

		status := strVal(s, "status")
		if status == "" {
			status = "ended"
		}
		model := strVal(s, "model")
		cat := strVal(s, "category")
		if cat == "" {
			cat = "-"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s%s\t%s\t%s\t%s\n",
			shortW, shortCC, turns, created, lastActive, name, marker, status, model, cat)
	}
	w.Flush()
}

// strVal safely extracts a string from a map[string]any, returning "" for nil or non-string.
func strVal(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}

// isSessionActive returns true if the session snapshot looks like an active session.
func isSessionActive(s map[string]any) bool {
	status, _ := s["status"].(string)
	return status == "active" || status == "idle" || status == "running" || status == "busy"
}

// getSessionTime returns the best available timestamp for sorting.
func getSessionTime(s map[string]any) time.Time {
	for _, key := range []string{"last_active", "updated_at", "mod_time", "created_at"} {
		if v, ok := s[key].(string); ok && v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// ipcSend sends a message from this session to a target session.
func ipcSend(targetID, message string) {
	_, _, myID := ipcEnv()
	if myID == "" {
		fmt.Fprintf(os.Stderr, "error: WEIRAN_SESSION_ID not set (required for send)\n")
		os.Exit(1)
	}

	// Resolve short ID to full ID
	targetID = resolveSessionID(targetID)

	body, _ := json.Marshal(map[string]string{
		"from_session_id": myID,
		"message":         message,
	})
	resp, err := ipcRequest("POST", "/api/sessions/"+targetID+"/message-from", strings.NewReader(string(body)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	if resp.StatusCode == http.StatusTooManyRequests {
		fmt.Fprintf(os.Stderr, "interaction limit reached (round %v/%v)\n",
			result["round"], result["max_rounds"])
		os.Exit(1)
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "error: %v\n", result["error"])
		os.Exit(1)
	}

	fmt.Fprintf(os.Stdout, "sent (round %v/%v)\n", result["round"], result["max_rounds"])
}

// ipcRead reads full message history of a target session.
// verbose=true shows full tool inputs/results; otherwise tool inputs are summarized.
// showTS=true prefixes each non-tool line with a short timestamp (HH:MM:SS).
func ipcRead(targetID string, verbose, showTS bool) {
	targetID = resolveSessionID(targetID)

	resp, err := ipcRequest("GET", "/api/history/"+targetID+"/messages?limit=200", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var messages []map[string]any
	json.NewDecoder(resp.Body).Decode(&messages)

	if len(messages) == 0 {
		fmt.Println("(no messages)")
		return
	}

	for _, m := range messages {
		role := fmt.Sprintf("%v", m["role"])
		content, _ := m["content"].(string)
		ts := ""
		if showTS {
			if v, ok := m["timestamp"].(string); ok && len(v) >= 19 {
				// ISO-8601 → HH:MM:SS
				ts = v[11:19] + " "
			}
		}

		switch role {
		case "user":
			fmt.Printf("%s🧑 %s\n\n", ts, content)
		case "assistant":
			fmt.Printf("%s🤖 %s\n\n", ts, content)
		case "tool_use":
			toolName, _ := m["tool_name"].(string)
			toolInput, _ := m["tool_input"].(string)
			if verbose {
				fmt.Printf("  🔧 %s\n", toolName)
				if toolInput != "" {
					fmt.Printf("%s\n", indent(toolInput, "     "))
				}
			} else {
				fmt.Printf("  🔧 %s(%s)\n", toolName, summarizeToolInput(toolName, toolInput))
			}
		case "tool_result":
			if content == "" {
				continue
			}
			if verbose {
				fmt.Printf("  ↪ %s\n\n", indent(content, "    "))
			} else {
				fmt.Printf("  ↪ %s\n\n", truncateOneLine(content, 160))
			}
		case "image":
			imgs, _ := m["images"].([]any)
			fmt.Printf("  🖼  [%d image(s)]\n\n", len(imgs))
		case "compact_boundary":
			// Content format: "compact_boundary|<trigger>|<tokens>"
			fmt.Printf("%s── compact boundary (%s) ──\n\n", ts, content)
		case "system":
			// skip silent system events
		default:
			fmt.Printf("[%s] %s\n\n", role, content)
		}
	}
}

// summarizeToolInput returns a short one-line summary of a tool's JSON input,
// highlighting the 1-2 most useful fields per tool.
func summarizeToolInput(toolName, rawInput string) string {
	if rawInput == "" {
		return ""
	}
	var in map[string]any
	if err := json.Unmarshal([]byte(rawInput), &in); err != nil {
		return truncateOneLine(rawInput, 120)
	}

	// Per-tool key picking for the most useful summary.
	pick := func(keys ...string) string {
		var parts []string
		for _, k := range keys {
			v, ok := in[k]
			if !ok || v == nil {
				continue
			}
			parts = append(parts, fmt.Sprintf("%s=%s", k, truncateOneLine(fmt.Sprintf("%v", v), 80)))
		}
		return strings.Join(parts, ", ")
	}

	switch toolName {
	case "Bash":
		if desc, _ := in["description"].(string); desc != "" {
			if cmd, _ := in["command"].(string); cmd != "" {
				return fmt.Sprintf("%q → %s", truncateOneLine(cmd, 60), desc)
			}
		}
		return pick("command")
	case "Read":
		return pick("file_path", "offset", "limit")
	case "Edit", "MultiEdit", "NotebookEdit":
		return pick("file_path", "notebook_path")
	case "Write":
		return pick("file_path")
	case "Grep":
		return pick("pattern", "path", "glob", "type")
	case "Glob":
		return pick("pattern", "path")
	case "WebFetch":
		return pick("url")
	case "WebSearch":
		return pick("query")
	case "TodoWrite":
		if todos, ok := in["todos"].([]any); ok {
			return fmt.Sprintf("%d todo(s)", len(todos))
		}
	case "Task", "Agent":
		return pick("description", "subagent_type")
	case "Skill":
		return pick("skill", "args")
	}

	// Fallback: dump first key=value we can stringify.
	for k, v := range in {
		return fmt.Sprintf("%s=%s", k, truncateOneLine(fmt.Sprintf("%v", v), 100))
	}
	return ""
}

// truncateOneLine collapses whitespace and truncates to max runes.
func truncateOneLine(s string, max int) string {
	s = strings.TrimSpace(s)
	// collapse internal newlines/tabs into spaces
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	// squeeze consecutive spaces
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// indent prefixes every line of s with prefix.
func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// ipcSearch searches a target session's history using FTS.
func ipcSearch(targetID, keyword string) {
	targetID = resolveSessionID(targetID)

	// Use the server's search API with content scope
	q := url.Values{}
	q.Set("q", keyword)
	q.Set("scope", "content")
	q.Set("limit", "20")
	resp, err := ipcRequest("GET", "/api/search?"+q.Encode(), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	hits, ok := result["hits"].([]any)
	if !ok || len(hits) == 0 {
		fmt.Println("(no matches)")
		return
	}

	// Filter hits for the target session
	matched := 0
	for _, h := range hits {
		hit, ok := h.(map[string]any)
		if !ok {
			continue
		}
		// Check if this hit is from the target session
		source := fmt.Sprintf("%v", hit["source"])
		if !strings.Contains(source, shortID(targetID)) && !strings.Contains(source, targetID) {
			continue
		}
		matched++
		snippet := fmt.Sprintf("%v", hit["snippet"])
		fmt.Printf("--- match %d (score: %v) ---\n%s\n\n", matched, hit["score"], snippet)
	}

	if matched == 0 {
		fmt.Println("(no matches in target session)")
	}
}

// resolveSessionID resolves a short ID prefix to a full session ID by listing active sessions.
// It matches against both weiran session ID and CC session ID.
// If multiple sessions match, it prints their metadata and exits so the caller can disambiguate.
func resolveSessionID(input string) string {
	// If it looks like a full UUID, use as-is
	if len(input) > 16 {
		return input
	}

	type candidate struct {
		id    string // weiran session ID
		ccSID string // CC session ID
		name  string
		model string
		time  string // created_at or last_active
	}

	var candidates []candidate

	// Check active sessions
	resp, err := ipcRequest("GET", "/api/sessions", nil)
	if err == nil {
		defer resp.Body.Close()
		var sessions []map[string]any
		json.NewDecoder(resp.Body).Decode(&sessions)

		for _, s := range sessions {
			id := fmt.Sprintf("%v", s["id"])
			ccSID := ""
			if v, ok := s["claude_session_id"]; ok && v != nil {
				ccSID = fmt.Sprintf("%v", v)
			}
			if strings.HasPrefix(id, input) || (ccSID != "" && strings.HasPrefix(ccSID, input)) {
				ts := ""
				if v, ok := s["created_at"]; ok && v != nil {
					ts = fmt.Sprintf("%v", v)
				}
				candidates = append(candidates, candidate{
					id: id, ccSID: ccSID,
					name:  fmt.Sprintf("%v", s["name"]),
					model: fmt.Sprintf("%v", s["model"]),
					time:  ts,
				})
			}
		}
	}

	// Also check history for non-active sessions
	resp2, err := ipcRequest("GET", "/api/history?limit=50", nil)
	if err == nil {
		defer resp2.Body.Close()
		var history []map[string]any
		json.NewDecoder(resp2.Body).Decode(&history)

		for _, s := range history {
			id := fmt.Sprintf("%v", s["id"])
			ccSID := ""
			if v, ok := s["claude_session_id"]; ok && v != nil {
				ccSID = fmt.Sprintf("%v", v)
			}
			// Skip if already in candidates (active session covers this CC session)
			dup := false
			for _, c := range candidates {
				if c.id == id || c.ccSID == id {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
			if strings.HasPrefix(id, input) || (ccSID != "" && strings.HasPrefix(ccSID, input)) {
				ts := ""
				if v, ok := s["created_at"]; ok && v != nil {
					ts = fmt.Sprintf("%v", v)
				}
				candidates = append(candidates, candidate{
					id: id, ccSID: ccSID,
					name:  fmt.Sprintf("%v", s["name"]),
					model: fmt.Sprintf("%v", s["model"]),
					time:  ts,
				})
			}
		}
	}

	if len(candidates) == 0 {
		return input // couldn't resolve, return as-is
	}
	if len(candidates) == 1 {
		return candidates[0].id
	}

	// Multiple matches — print disambiguation table to stderr and return first match.
	// Callers see the resolution succeed but the user gets a warning to use a longer prefix.
	fmt.Fprintf(os.Stderr, "prefix %q matches %d sessions (using first match):\n\n", input, len(candidates))
	w := tabwriter.NewWriter(os.Stderr, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tCC_SID\tNAME\tMODEL\tCREATED")
	for _, c := range candidates {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			shortID(c.id), shortID(c.ccSID), c.name, c.model, c.time)
	}
	w.Flush()
	fmt.Fprintf(os.Stderr, "\ntip: use a longer prefix to disambiguate\n")
	return candidates[0].id
}

// ipcClose destroys a target session via DELETE /api/sessions/{id}.
func ipcClose(targetID string) {
	targetID = resolveSessionID(targetID)

	// Safety: don't close yourself
	_, _, myID := ipcEnv()
	if targetID == myID {
		fmt.Fprintf(os.Stderr, "error: cannot close your own session\n")
		os.Exit(1)
	}

	resp, err := ipcRequest("DELETE", "/api/sessions/"+targetID, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "error: %v (status %d)\n", result["error"], resp.StatusCode)
		os.Exit(1)
	}

	fmt.Printf("session %s closed\n", shortID(targetID))
}

// ipcPrepareRestart marks the current session for rehydration after server restart.
// The session will be resumed and receive a wake message after the server comes back.
func ipcPrepareRestart(message string) {
	_, _, myID := ipcEnv()
	if myID == "" {
		fmt.Fprintf(os.Stderr, "error: %s_SESSION_ID not set\n", ipcEnvPrefix())
		os.Exit(1)
	}

	if message == "" {
		message = "Server restarted successfully. Continue from where you left off."
	}

	body, _ := json.Marshal(map[string]string{
		"session_id": myID,
		"message":    message,
	})
	resp, err := ipcRequest("POST", "/api/server/prepare-restart", strings.NewReader(string(body)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to decode response (status %d): %v\n", resp.StatusCode, err)
		os.Exit(1)
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "error: %v (status %d)\n", result["error"], resp.StatusCode)
		os.Exit(1)
	}

	fmt.Printf("prepared for restart (session %s → claude %s)\n",
		shortID(myID), shortID(fmt.Sprintf("%v", result["claude_session_id"])))
}

// ipcWait blocks until the target session reaches idle/stopped/error state.
func ipcWait(targetID string) {
	targetID = resolveSessionID(targetID)

	fmt.Fprintf(os.Stderr, "waiting for session %s...\n", shortID(targetID))

	resp, err := ipcRequest("GET", "/api/sessions/"+targetID+"/wait", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to decode response (status %d): %v\n", resp.StatusCode, err)
		os.Exit(1)
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "error: %v (status %d)\n", result["error"], resp.StatusCode)
		os.Exit(1)
	}

	status := fmt.Sprintf("%v", result["status"])
	turns := result["num_turns"]
	timedOut, _ := result["timeout"].(bool)

	if timedOut {
		fmt.Printf("timeout (status: %s, turns: %v)\n", status, turns)
	} else {
		fmt.Printf("done (status: %s, turns: %v)\n", status, turns)
	}
}
