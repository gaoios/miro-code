package app

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"
)

// runningTask represents a long-running background task tracked per session.
// Sources: Bash run_in_background:true, Monitor, Agent run_in_background:true.
// Lifecycle:
//   - created when a tool_use launching a background task is observed
//   - updated whenever a <task-notification> matching its task-id arrives
//   - terminal when the notification carries a final marker (output-file present
//     for bash/agent, or "completed"/"failed" in summary)
type runningTask struct {
	ID          string    `json:"id"`                     // task-id; falls back to tool_use_id when missing
	ToolUseID   string    `json:"tool_use_id,omitempty"`  // originating tool_use id (for dedupe)
	Type        string    `json:"type"`                   // bash | monitor | agent
	Description string    `json:"description"`            // human-readable label
	StartedAt   time.Time `json:"started_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Status      string    `json:"status"`            // running | done | failed | cancelled
	EventCount  int       `json:"event_count"`       // number of <event> notifications received
	LastEvent   string    `json:"last_event,omitempty"`
	OutputFile  string    `json:"output_file,omitempty"`
}

// taskTracker is a per-session in-memory registry of background tasks.
//
// We index by both task-id and tool_use_id because tasks are created from a
// tool_use payload (where we only know tool_use_id) and later get updates from
// <task-notification> blocks (which carry both task-id and tool_use_id, but the
// task-id is the canonical reference).
type taskTracker struct {
	mu        sync.Mutex
	byID      map[string]*runningTask // primary key: task-id (or tool_use_id while task-id unknown)
	byToolUse map[string]string       // tool_use_id → task-id (for forwarding updates back to the canonical entry)
	max       int                     // soft cap; oldest done tasks evicted when exceeded
}

func newTaskTracker() *taskTracker {
	return &taskTracker{
		byID:      make(map[string]*runningTask),
		byToolUse: make(map[string]string),
		max:       100,
	}
}

// snapshot returns a JSON-safe copy of all tasks, newest first by StartedAt.
func (t *taskTracker) snapshot() []*runningTask {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*runningTask, 0, len(t.byID))
	for _, task := range t.byID {
		cp := *task
		out = append(out, &cp)
	}
	// Sort newest-first
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].StartedAt.After(out[j-1].StartedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// gcLocked evicts oldest done/failed tasks if we exceed the cap.
// Caller must hold t.mu.
func (t *taskTracker) gcLocked() {
	if len(t.byID) <= t.max {
		return
	}
	// Find oldest terminal task and evict it (one per call is fine; called on every insert)
	var oldestID string
	var oldestTime time.Time
	for id, task := range t.byID {
		if task.Status == "running" {
			continue
		}
		if oldestID == "" || task.UpdatedAt.Before(oldestTime) {
			oldestID = id
			oldestTime = task.UpdatedAt
		}
	}
	if oldestID != "" {
		if task, ok := t.byID[oldestID]; ok {
			delete(t.byToolUse, task.ToolUseID)
		}
		delete(t.byID, oldestID)
	}
}

// trackToolUse inspects a tool_use payload and creates a runningTask if it
// represents a background-launching tool. Returns the created/updated task or
// nil if this tool_use is not a background launcher.
func (t *taskTracker) trackToolUse(raw json.RawMessage) *runningTask {
	if t == nil {
		return nil
	}
	var peek struct {
		Message struct {
			Content []struct {
				Type      string          `json:"type"`
				Name      string          `json:"name"`
				ID        string          `json:"id"`
				Input     json.RawMessage `json:"input"`
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
		var taskType, desc string
		switch c.Name {
		case "Bash":
			var inp struct {
				Command          string `json:"command"`
				Description      string `json:"description"`
				RunInBackground  bool   `json:"run_in_background"`
			}
			_ = json.Unmarshal(c.Input, &inp)
			if !inp.RunInBackground {
				continue
			}
			taskType = "bash"
			desc = inp.Description
			if desc == "" {
				desc = truncateStr(inp.Command, 80)
			}
		case "Monitor":
			var inp struct {
				Description string `json:"description"`
				Command     string `json:"command"`
			}
			_ = json.Unmarshal(c.Input, &inp)
			taskType = "monitor"
			desc = inp.Description
			if desc == "" {
				desc = truncateStr(inp.Command, 80)
			}
		case "Agent":
			var inp struct {
				Description     string `json:"description"`
				SubagentType    string `json:"subagent_type"`
				RunInBackground bool   `json:"run_in_background"`
			}
			_ = json.Unmarshal(c.Input, &inp)
			if !inp.RunInBackground {
				continue
			}
			taskType = "agent"
			desc = inp.Description
			if desc == "" {
				desc = inp.SubagentType
			}
			if desc == "" {
				desc = "Agent task"
			}
		default:
			continue
		}
		if c.ID == "" {
			continue
		}
		now := time.Now()
		task := &runningTask{
			ID:          c.ID, // provisional; replaced by task-id when first notification arrives
			ToolUseID:   c.ID,
			Type:        taskType,
			Description: desc,
			StartedAt:   now,
			UpdatedAt:   now,
			Status:      "running",
		}
		t.mu.Lock()
		t.byID[c.ID] = task
		t.byToolUse[c.ID] = c.ID
		t.gcLocked()
		t.mu.Unlock()
		return task
	}
	return nil
}

// taskNotifRe extracts <task-notification> blocks from tool_result text.
var taskNotifRe = regexp.MustCompile(`(?s)<task-notification>(.*?)</task-notification>`)

// taskNotifFieldRe extracts a named tag from inside a notification body.
func notifField(body, tag string) string {
	re := regexp.MustCompile(`(?s)<` + tag + `>(.*?)</` + tag + `>`)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// trackToolResult scans a tool_result payload for <task-notification> blocks
// and updates the corresponding tasks. Returns the list of updated tasks
// (caller broadcasts task_event SSE events for each).
func (t *taskTracker) trackToolResult(raw json.RawMessage) []*runningTask {
	if t == nil {
		return nil
	}
	// Pull the tool_result text out. tool_results live under message.content[].content
	// when they come from the user side (after Claude Code injects the notification).
	var peek struct {
		Message struct {
			Content []struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &peek) != nil {
		return nil
	}
	var bodies []string
	for _, c := range peek.Message.Content {
		if c.Type != "tool_result" {
			continue
		}
		// content is either a string or an array of {type:"text", text:"..."}
		var asString string
		if json.Unmarshal(c.Content, &asString) == nil && asString != "" {
			bodies = append(bodies, asString)
			continue
		}
		var asBlocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(c.Content, &asBlocks) == nil {
			for _, b := range asBlocks {
				if b.Text != "" {
					bodies = append(bodies, b.Text)
				}
			}
		}
	}
	if len(bodies) == 0 {
		return nil
	}
	var updated []*runningTask
	for _, body := range bodies {
		matches := taskNotifRe.FindAllStringSubmatch(body, -1)
		for _, m := range matches {
			inner := m[1]
			taskID := notifField(inner, "task-id")
			toolUseID := notifField(inner, "tool-use-id")
			summary := notifField(inner, "summary")
			event := notifField(inner, "event")
			outputFile := notifField(inner, "output-file")
			if task := t.applyNotif(taskID, toolUseID, summary, event, outputFile); task != nil {
				updated = append(updated, task)
			}
		}
	}
	return updated
}

// applyNotif merges a single notification into the tracker, creating a new
// entry if needed. Returns a snapshot of the updated task.
func (t *taskTracker) applyNotif(taskID, toolUseID, summary, event, outputFile string) *runningTask {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// Resolve canonical task entry: prefer task-id, fall back to tool_use_id mapping.
	var task *runningTask
	if taskID != "" {
		task = t.byID[taskID]
	}
	if task == nil && toolUseID != "" {
		if mapped, ok := t.byToolUse[toolUseID]; ok {
			task = t.byID[mapped]
		}
	}
	now := time.Now()
	if task == nil {
		// Notification arrived without a prior tool_use (e.g. server restarted
		// mid-task). Create a stub so the UI still reflects the activity.
		key := taskID
		if key == "" {
			key = toolUseID
		}
		if key == "" {
			return nil
		}
		task = &runningTask{
			ID:          key,
			ToolUseID:   toolUseID,
			Type:        "background",
			Description: summary,
			StartedAt:   now,
			Status:      "running",
		}
		t.byID[key] = task
		if toolUseID != "" {
			t.byToolUse[toolUseID] = key
		}
		t.gcLocked()
	}
	// If we now know a real task-id and the entry was originally keyed under
	// a tool_use_id placeholder, re-key it.
	if taskID != "" && task.ID != taskID {
		delete(t.byID, task.ID)
		task.ID = taskID
		t.byID[taskID] = task
		if toolUseID != "" {
			t.byToolUse[toolUseID] = taskID
		}
	}
	task.UpdatedAt = now
	task.EventCount++
	if event != "" {
		task.LastEvent = truncateStr(event, 500)
	} else if summary != "" {
		task.LastEvent = truncateStr(summary, 500)
	}
	if outputFile != "" {
		task.OutputFile = outputFile
		// output-file present generally means the task finished; let summary
		// downgrade to failure when it indicates that.
		task.Status = classifyTerminal(summary, "done")
	} else if isTerminalSummary(summary) {
		task.Status = classifyTerminal(summary, "done")
	}
	cp := *task
	return &cp
}

// markFailed flips a task to failed (used when the bridge detects a process exit).
func (t *taskTracker) markAllRunningAsCancelled() []*runningTask {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	var changed []*runningTask
	for _, task := range t.byID {
		if task.Status == "running" {
			task.Status = "cancelled"
			task.UpdatedAt = now
			cp := *task
			changed = append(changed, &cp)
		}
	}
	return changed
}

func isTerminalSummary(s string) bool {
	if s == "" {
		return false
	}
	low := strings.ToLower(s)
	for _, marker := range []string{"completed", "finished", "exited", "failed", "error", "killed", "timeout"} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

func classifyTerminal(summary, def string) string {
	low := strings.ToLower(summary)
	if strings.Contains(low, "fail") || strings.Contains(low, "error") || strings.Contains(low, "killed") {
		return "failed"
	}
	if strings.Contains(low, "timeout") {
		return "failed"
	}
	return def
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
