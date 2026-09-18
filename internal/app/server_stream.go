package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	sseKeepaliveInterval = 15 * time.Second
	subscriberBufSize    = 256
)

// sseEvent is one Server-Sent Event to push to a client.
type sseEvent struct {
	Event string // SSE event type (init, assistant, tool_use, tool_result, result, error, status)
	Data  []byte // JSON payload
}

// subscriber is a single SSE client connection.
type subscriber struct {
	ch     chan sseEvent
	closed chan struct{}
}

// sseBroadcaster fans out events to all subscribers (SSE + WebSocket).
type sseBroadcaster struct {
	mu            sync.RWMutex
	subscribers   map[*subscriber]struct{}
	history       []sseEvent // ring buffer of recent events for late joiners
	historyMax    int
	hub           *wsHub // also push to WebSocket hub (nil = SSE only)
	sessionID     string // server session ID for WS routing
	lastEventTime time.Time // last event broadcast time (for activity detection)
}

func newBroadcaster() *sseBroadcaster {
	return &sseBroadcaster{
		subscribers:   make(map[*subscriber]struct{}),
		historyMax:    100,
		lastEventTime: time.Now(), // initialize so empty sessions are detected as idle
	}
}

// subscribe creates a new subscriber. Caller must call unsubscribe when done.
func (b *sseBroadcaster) subscribe() *subscriber {
	sub := &subscriber{
		ch:     make(chan sseEvent, subscriberBufSize),
		closed: make(chan struct{}),
	}

	b.mu.Lock()
	// Send history to new subscriber (catch-up)
	for _, ev := range b.history {
		select {
		case sub.ch <- ev:
		default:
			// buffer full, skip old history
		}
	}
	b.subscribers[sub] = struct{}{}
	b.mu.Unlock()

	return sub
}

// unsubscribe removes a subscriber and closes its channel.
func (b *sseBroadcaster) unsubscribe(sub *subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subscribers[sub]; ok {
		delete(b.subscribers, sub)
		close(sub.closed)
	}
}

// broadcast sends an event to all subscribers. Slow consumers are dropped.
// NOTE: hub.broadcastSessionEvent is called OUTSIDE b.mu to avoid deadlock:
// broadcast holds broadcaster.mu → hub call → notifySessions → snapshot → broadcaster.count (needs broadcaster.mu).
func (b *sseBroadcaster) broadcast(ev sseEvent) {
	b.mu.Lock()

	// Append to history ring buffer
	b.history = append(b.history, ev)
	if len(b.history) > b.historyMax {
		b.history = b.history[len(b.history)-b.historyMax:]
	}
	b.lastEventTime = time.Now()

	for sub := range b.subscribers {
		select {
		case sub.ch <- ev:
		default:
			// Slow consumer: drop and disconnect
			delete(b.subscribers, sub)
			close(sub.closed)
		}
	}

	// Capture hub/sessionID under lock, call outside
	hub := b.hub
	sid := b.sessionID
	b.mu.Unlock()

	// Push to WebSocket hub OUTSIDE broadcaster lock (avoids deadlock cycle)
	if hub != nil && sid != "" {
		hub.broadcastSessionEvent(sid, ev.Event, ev.Data)
	}
}

// count returns the number of active subscribers.
func (b *sseBroadcaster) count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// lastEventAt returns the time of the last broadcast event.
func (b *sseBroadcaster) lastEventAt() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.lastEventTime.IsZero() {
		return ""
	}
	return b.lastEventTime.Format(time.RFC3339)
}

// idleSeconds returns seconds since the last broadcast event.
// Returns -1 if no events have been broadcast yet.
func (b *sseBroadcaster) idleSeconds() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.lastEventTime.IsZero() {
		return -1
	}
	return int(time.Since(b.lastEventTime).Seconds())
}

// ── SSE HTTP handler ──

// serveSSE writes Server-Sent Events to the HTTP response.
// Blocks until client disconnects or session ends.
func serveSSE(w http.ResponseWriter, r *http.Request, broadcaster *sseBroadcaster) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx passthrough
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub := broadcaster.subscribe()
	defer broadcaster.unsubscribe(sub)

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.closed:
			// Dropped by broadcaster (slow consumer) or session ended
			return
		case ev := <-sub.ch:
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Event, ev.Data)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// ── stdout → SSE bridge ──

// bridgeStdout reads Claude Code stdout and broadcasts as SSE events.
// Also updates session status based on message types.
// Blocks until the process stdout is closed.
//
// onCanUseTool is invoked when the subprocess sends a control_request with
// subtype=can_use_tool (triggered by --permission-prompt-tool stdio). The
// callback is responsible for sending a matching control_response via the
// process stdin — usually by recording a pending entry and waking up the
// Web UI through an ask_user_question SSE event. If the callback is nil
// or returns false, bridgeStdout auto-allows the tool (preserving prior
// bypassPermissions behavior for tools that unexpectedly reach this path).
func bridgeStdout(proc *claudeBackend, broadcaster *sseBroadcaster, onInit func(json.RawMessage), onResult func(json.RawMessage), onTodos func(json.RawMessage), onMemoryAudit func(*memoryAuditEntry), onCanUseTool func(json.RawMessage) bool, onTask func(event string, raw json.RawMessage), onUsage func(total int64), onSegmentComplete func(text string)) {
	// Track pending memory tool_use ops waiting for their tool_result
	pendingMemOps := make(map[string]*pendingMemoryOp)

	// Accumulate assistant text for content-hook segment detection.
	// Reset on tool_use (segment boundary). Evaluated when tool_use arrives.
	var segmentBuf strings.Builder

	proc.readLines(func(msgType string, raw json.RawMessage) {
		// Route control_response to sync waiters
		if msgType == "control_response" {
			proc.deliverResponse(raw)
		}

		// Intercept can_use_tool permission requests and hand them to the
		// session's AUQ handler. See the function doc for fallback rules.
		if msgType == "control_request" {
			handleCanUseTool(proc, raw, onCanUseTool)
		}

		event := mapEventType(msgType, raw)
		broadcaster.broadcast(sseEvent{Event: event, Data: raw})

		if msgType == "system" {
			var peek struct {
				Subtype string `json:"subtype"`
			}
			if json.Unmarshal(raw, &peek) == nil && peek.Subtype == "init" {
				proc.signalInit()
				if onInit != nil {
					onInit(raw)
				}
			}
		}
		if msgType == "result" && onResult != nil {
			onResult(raw)
		}

		// Detect TodoWrite tool_use and extract todos for cross-session broadcast
		if event == "tool_use" && onTodos != nil {
			extractTodoWrite(raw, onTodos)
		}

		// Track peak context tokens from assistant message usage. This mirrors
		// the frontend's handleAssistant() total calculation so the server can
		// surface peak usage in session snapshots (survives resume/page reload/
		// session switch). Fires for both "assistant" and "tool_use" mapped
		// events — both are assistant messages with usage.
		if onUsage != nil && msgType == "assistant" {
			if total := extractAssistantContextTotal(raw); total > 0 {
				onUsage(total)
			}
		}

		// PostAssistantMsg content hook: accumulate assistant text segments.
		// When a tool_use arrives, the preceding text is a complete mid-turn
		// segment — evaluate content hook rules against it.
		if event == "assistant" {
			if text := extractAssistantText(raw); text != "" {
				segmentBuf.WriteString(text)
			}
		}
		if event == "tool_use" && onSegmentComplete != nil && segmentBuf.Len() > 0 {
			onSegmentComplete(segmentBuf.String())
			segmentBuf.Reset()
		}
		// On result (turn end), clear segment buf without evaluating —
		// msg3 (final segment) is left for the official Stop hook.
		if msgType == "result" {
			segmentBuf.Reset()
		}

		// Forward tool_use / tool_result to the per-session task tracker so it
		// can build a snapshot of running background tasks (Bash run_in_background,
		// Monitor, async Agent) and capture <task-notification> updates.
		if onTask != nil && (event == "tool_use" || event == "tool_result") {
			onTask(event, raw)
		}

		// Memory audit: track tool_use targeting memory paths
		if event == "tool_use" && onMemoryAudit != nil {
			if pending := extractMemoryToolUse(raw); pending != nil {
				pendingMemOps[pending.ToolUseID] = pending
			}
			// Evict stale pending ops (>5min without matching tool_result)
			if len(pendingMemOps) > 20 {
				staleThreshold := time.Now().Add(-5 * time.Minute)
				for id, op := range pendingMemOps {
					if op.StartTime.Before(staleThreshold) {
						delete(pendingMemOps, id)
					}
				}
			}
		}

		// Memory audit: match tool_result back to pending memory ops
		if event == "tool_result" && onMemoryAudit != nil && len(pendingMemOps) > 0 {
			if entry := matchMemoryToolResult(raw, pendingMemOps); entry != nil {
				onMemoryAudit(entry)
			}
		}
	})

	// Process exited — notify subscribers, unless this exit was an
	// intentional reload (e.g. chrome toggle).
	if !proc.suppressClose.Load() {
		broadcaster.broadcast(sseEvent{
			Event: "close",
			Data:  []byte(`{"reason":"process_exited"}`),
		})
	}
}

// handleCanUseTool inspects a control_request and, if it carries a
// can_use_tool permission query, hands it to the session-scoped onCanUseTool
// callback. If the callback claims the request (returns true) it is also
// responsible for eventually writing the matching control_response. For
// any other outcome (non-can_use_tool, unrecognized tool, callback absent
// or returns false), auto-allow so bypassPermissions mode keeps working.
func handleCanUseTool(proc *claudeBackend, raw json.RawMessage, onCanUseTool func(json.RawMessage) bool) {
	var peek struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype  string          `json:"subtype"`
			ToolName string          `json:"tool_name"`
			Input    json.RawMessage `json:"input"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return
	}
	if peek.Request.Subtype != "can_use_tool" || peek.RequestID == "" {
		return
	}

	if onCanUseTool != nil && onCanUseTool(raw) {
		return
	}

	// Fallback: auto-allow with the original input. This keeps tools that
	// slipped past bypassPermissions-auto-allow from deadlocking the turn.
	decision := map[string]any{
		"behavior":     "allow",
		"updatedInput": json.RawMessage(peek.Request.Input),
	}
	if len(peek.Request.Input) == 0 {
		decision["updatedInput"] = map[string]any{}
	}
	if err := proc.sendPermissionDecision(peek.RequestID, decision); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] auto-allow permission failed for %s: %v\n", appName, peek.RequestID, err)
	}
}

// extractAssistantContextTotal reads message.usage from an assistant stream-json
// event and returns input_tokens + cache_creation_input_tokens +
// cache_read_input_tokens (0 when usage is absent or malformed). This matches
// the total the Web UI uses to drive the context bar.
func extractAssistantContextTotal(raw json.RawMessage) int64 {
	var peek struct {
		Message struct {
			Usage struct {
				InputTokens              int64 `json:"input_tokens"`
				CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return 0
	}
	u := peek.Message.Usage
	total := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	if total < 0 {
		return 0
	}
	return total
}

// extractTodoWrite parses a tool_use event for TodoWrite and calls onTodos with the todos array.
func extractTodoWrite(raw json.RawMessage, onTodos func(json.RawMessage)) {
	var peek struct {
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &peek) != nil {
		return
	}
	for _, c := range peek.Message.Content {
		if c.Type == "tool_use" && c.Name == "TodoWrite" {
			var inp struct {
				Todos json.RawMessage `json:"todos"`
			}
			if json.Unmarshal(c.Input, &inp) == nil && len(inp.Todos) > 0 {
				onTodos(inp.Todos)
			}
			return
		}
	}
}

// mapEventType maps stream-json type to SSE event name.
// For assistant messages, peek at content to distinguish tool_use vs text.
func mapEventType(msgType string, raw json.RawMessage) string {
	switch msgType {
	case "system":
		var peek struct {
			Subtype string `json:"subtype"`
		}
		json.Unmarshal(raw, &peek)
		if peek.Subtype == "init" {
			return "init"
		}
		return "system"
	case "assistant":
		// Check if this contains tool_use
		var peek struct {
			Message struct {
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &peek) == nil {
			for _, c := range peek.Message.Content {
				if c.Type == "tool_use" {
					return "tool_use"
				}
			}
		}
		return "assistant"
	case "user":
		return "tool_result"
	case "result":
		return "result"
	default:
		return msgType
	}
}

// extractAssistantText pulls concatenated text content from an assistant
// stream-json event. Returns "" if no text blocks or parse failure.
func extractAssistantText(raw json.RawMessage) string {
	var peek struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &peek) != nil {
		return ""
	}
	var sb strings.Builder
	for _, c := range peek.Message.Content {
		if c.Type == "text" && c.Text != "" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}
