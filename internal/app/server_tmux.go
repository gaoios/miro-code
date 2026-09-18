package app

// Tmux session snooping for the Web UI drawer.
//
// When the user doesn't use tmux directly, every session visible here is the
// agent itself (or tasks it has spawned) — opened for long-task observability.
// Read-only; the UI does not expose kill/attach.

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type tmuxWindow struct {
	Index          int    `json:"index"`
	Name           string `json:"name"`
	Active         bool   `json:"active"`
	Panes          int    `json:"panes"`
	CurrentCommand string `json:"current_command,omitempty"`
	CurrentPath    string `json:"current_path,omitempty"`
	// Web-service protocol (tmux user-options @web_port / @web_url / @web_label).
	// When a window exposes an HTTP service, the agent sets these options so the Web UI
	// can render an "Open" button instead of a raw pane preview. See
	// memory/topics/feedback_web_service_in_tmux.md for the startup convention.
	WebPort  int    `json:"web_port,omitempty"`
	WebURL   string `json:"web_url,omitempty"`
	WebLabel string `json:"web_label,omitempty"`
	// Generic labels — all tmux @user-options on this window except the
	// reserved web-protocol keys (@web_port, @web_url, @web_label).
	// Collected automatically by scanning `tmux show-options -w`.
	// The Web UI renders each as a small pill badge.
	// Usage:  tmux set-option -t sess:win -w @cluster "tc-ap-beijing-1"
	//         tmux set-option -t sess:win -w @instance "in01-xxx"
	// No schema — any @key works, UI picks them up on next poll.
	Labels map[string]string `json:"labels,omitempty"`
}

type tmuxSession struct {
	Name     string       `json:"name"`
	Attached bool         `json:"attached"`
	Created  int64        `json:"created"`  // unix seconds
	Activity int64        `json:"activity"` // unix seconds (last activity)
	Windows  []tmuxWindow `json:"windows"`
}

type tmuxListResult struct {
	Available bool          `json:"available"` // tmux binary found AND server running
	Sessions  []tmuxSession `json:"sessions"`
	Error     string        `json:"error,omitempty"`
}

// listTmuxSessions shells out to tmux to enumerate sessions + windows.
// Never blocks longer than 3s. Returns Available=false if tmux isn't installed
// or no server is running.
func listTmuxSessions() tmuxListResult {
	if _, err := exec.LookPath("tmux"); err != nil {
		return tmuxListResult{Available: false, Sessions: []tmuxSession{}}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Sessions
	sessFmt := "#{session_name}|#{session_attached}|#{session_created}|#{session_activity}"
	out, err := exec.CommandContext(ctx, "tmux", "list-sessions", "-F", sessFmt).Output()
	if err != nil {
		// Exit code 1 with "no server running" stderr is the normal empty case.
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		if strings.Contains(msg, "no server running") || strings.Contains(msg, "error connecting") || msg == "" {
			return tmuxListResult{Available: true, Sessions: []tmuxSession{}}
		}
		return tmuxListResult{Available: true, Sessions: []tmuxSession{}, Error: msg}
	}

	sessByName := map[string]*tmuxSession{}
	var order []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 4 {
			continue
		}
		s := tmuxSession{
			Name:     parts[0],
			Attached: parts[1] != "0",
			Created:  parseInt64(parts[2]),
			Activity: parseInt64(parts[3]),
			Windows:  []tmuxWindow{},
		}
		sessByName[s.Name] = &s
		order = append(order, s.Name)
	}

	if len(order) == 0 {
		return tmuxListResult{Available: true, Sessions: []tmuxSession{}}
	}

	// Windows (all sessions in one call). Web-protocol fields (@web_port etc.)
	// are extracted from the format string; generic @labels are collected
	// separately via `tmux show-options -wv` per window.
	winFmt := "#{session_name}|#{window_index}|#{window_name}|#{window_active}|#{window_panes}|#{pane_current_command}|#{pane_current_path}|#{@web_port}|#{@web_url}|#{@web_label}"
	wout, werr := exec.CommandContext(ctx, "tmux", "list-windows", "-a", "-F", winFmt).Output()
	if werr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(wout)), "\n") {
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "|", 10)
			if len(parts) < 7 {
				continue
			}
			s, ok := sessByName[parts[0]]
			if !ok {
				continue
			}
			w := tmuxWindow{
				Index:          int(parseInt64(parts[1])),
				Name:           parts[2],
				Active:         parts[3] != "0",
				Panes:          int(parseInt64(parts[4])),
				CurrentCommand: parts[5],
				CurrentPath:    parts[6],
			}
			if len(parts) >= 10 {
				if port, err := strconv.Atoi(strings.TrimSpace(parts[7])); err == nil && port > 0 && port < 65536 {
					w.WebPort = port
				}
				w.WebURL = strings.TrimSpace(parts[8])
				w.WebLabel = strings.TrimSpace(parts[9])
			}
			// Collect all @user-options on this window as generic labels.
			w.Labels = collectWindowLabels(ctx, parts[0], w.Index)
			s.Windows = append(s.Windows, w)
		}
	}

	sessions := make([]tmuxSession, 0, len(order))
	for _, name := range order {
		sessions = append(sessions, *sessByName[name])
	}
	return tmuxListResult{Available: true, Sessions: sessions}
}

// collectWindowLabels runs `tmux show-options -w -t sess:idx` and returns all
// @user-options as a map, excluding the reserved web-protocol keys.
// Returns nil (omitted from JSON) if no custom labels are set.
func collectWindowLabels(ctx context.Context, session string, winIdx int) map[string]string {
	target := session + ":" + strconv.Itoa(winIdx)
	out, err := exec.CommandContext(ctx, "tmux", "show-options", "-w", "-t", target).Output()
	if err != nil {
		return nil
	}
	reserved := map[string]bool{"@web_port": true, "@web_url": true, "@web_label": true, "@web_title": true}
	labels := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "@") {
			continue
		}
		// Format: @key "value" or @key value
		sp := strings.SplitN(line, " ", 2)
		if len(sp) < 2 {
			continue
		}
		key := sp[0]
		if reserved[key] {
			continue
		}
		val := strings.Trim(strings.TrimSpace(sp[1]), "\"")
		labels[strings.TrimPrefix(key, "@")] = val
	}
	if len(labels) == 0 {
		return nil
	}
	return labels
}

// captureTmuxPane runs `tmux capture-pane -p -J -S -<lines>` on the given target.
// Target format is typically "session:window" or "session:window.pane".
// lines is clamped to [10, 500].
func captureTmuxPane(target string, lines int) (string, error) {
	if _, err := exec.LookPath("tmux"); err != nil {
		return "", err
	}
	if lines < 10 {
		lines = 10
	} else if lines > 500 {
		lines = 500
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "tmux", "capture-pane",
		"-p",                       // print to stdout
		"-J",                       // join wrapped lines
		"-S", "-"+strconv.Itoa(lines), // start N lines back
		"-t", target,
	).Output()
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", &captureErr{msg: msg}
	}
	return string(out), nil
}

type captureErr struct{ msg string }

func (e *captureErr) Error() string { return e.msg }

func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// ── HTTP handlers ──

func handleTmuxSessions(w http.ResponseWriter, r *http.Request) {
	_ = r
	res := listTmuxSessions()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// Validate tmux target to allow only safe characters.
// Allowed: letters, digits, dash, underscore, dot, colon.
func validTmuxTarget(t string) bool {
	if t == "" || len(t) > 128 {
		return false
	}
	for _, r := range t {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ':':
			// ok
		default:
			return false
		}
	}
	return true
}

func handleTmuxCapture(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if !validTmuxTarget(target) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid target"})
		return
	}
	lines := 80
	if ls := r.URL.Query().Get("lines"); ls != "" {
		if n, err := strconv.Atoi(ls); err == nil {
			lines = n
		}
	}
	out, err := captureTmuxPane(target, lines)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"target":  target,
			"content": "",
			"error":   err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"target":  target,
		"lines":   lines,
		"content": out,
	})
}
