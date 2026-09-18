package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// expandHome expands a leading ~ to the user's home directory.
func expandHome(path string) (string, error) {
	if !strings.HasPrefix(path, "~/") && path != "~" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path, fmt.Errorf("cannot determine home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

// ── Session ──

// Session categories determine lifecycle behavior.
const (
	CategoryInteractive = "interactive" // Web UI manual sessions — subject to maxSessions, normal TTL
	CategoryHeartbeat   = "heartbeat"   // Wake/heartbeat — auto-destroy on completion, not counted toward maxSessions
	CategoryCron        = "cron"        // Scheduled tasks — auto-destroy on completion
	CategoryEvolve      = "evolve"      // Daily self-evolution — auto-destroy on completion
)

// isEphemeralCategory returns true if sessions in this category should auto-destroy when done.
func isEphemeralCategory(cat string) bool {
	return cat == CategoryHeartbeat || cat == CategoryCron || cat == CategoryEvolve
}

// Session "mode" is a UI-level enum over two orthogonal persistence flags:
//
//	weiran (default) — soul=true,  replace=false → CC harness + SOUL appended
//	benwo (本我)      — soul=true,  replace=true  → SOUL replaces CC harness
//	cc    (bare)     — soul=false, replace=ignored → pure Claude Code, no SOUL
//
// The dead combo soul=false+replace=true is normalized to cc.
const (
	ModeWeiran = "weiran"
	ModeBenwo  = "benwo"
	ModeCC     = "cc"
)

// modeToFlags returns (soulEnabled, replaceSoul, ok). Unknown modes return ok=false
// so callers can fall back to legacy bool fields.
func modeToFlags(mode string) (bool, bool, bool) {
	switch mode {
	case ModeWeiran:
		return true, false, true
	case ModeBenwo:
		return true, true, true
	case ModeCC:
		return false, false, true
	}
	return false, false, false
}

// flagsToMode is the inverse. Dead combo (soul=false,replace=true) → cc.
func flagsToMode(soul, replace bool) string {
	if !soul {
		return ModeCC
	}
	if replace {
		return ModeBenwo
	}
	return ModeWeiran
}

// serverSession represents one active Claude Code session.
type serverSession struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Project       string          `json:"project"`
	Model         string          `json:"model,omitempty"`
	Status        string          `json:"status"`         // starting, running, idle, stopped, error
	Category      string          `json:"category"`       // interactive, heartbeat, cron, evolve
	Tags          []string        `json:"tags,omitempty"` // freeform labels for filtering
	CreatedAt     time.Time       `json:"created_at"`
	LastActive    time.Time       `json:"last_active"`
	TotalCost     float64         `json:"total_cost_usd"`
	NumTurns      int             `json:"num_turns"`
	ClaudeSID     string          `json:"claude_session_id,omitempty"` // Claude Code's own session ID
	ResumedFrom   string          `json:"resumed_from,omitempty"`      // Original session ID being resumed
	StreamURL     string          `json:"stream_url"`
	SoulEnabled   bool            `json:"soul_enabled"`
	ChromeEnabled bool            `json:"chrome_enabled"`
	Effort        string          `json:"effort,omitempty"`     // CC reasoning effort tier (low/medium/high/xhigh/max), persisted
	ReplaceSoul   bool            `json:"replace_soul"`         // 本我模式 — use --system-prompt-file (replace) instead of --append-system-prompt-file
	FirstMsg      string          `json:"first_msg,omitempty"`  // First user message (for hint display)
	GalID         string          `json:"gal_id,omitempty"`     // GAL save id this session was resumed from
	Todos         json.RawMessage `json:"todos,omitempty"`      // latest TodoWrite state (broadcast to all clients)
	SpawnedBy     string          `json:"spawned_by,omitempty"` // parent session ID that spawned this one

	// PeakContextTokens tracks the high-water mark of input tokens consumed by
	// this session (input + cache_creation + cache_read). Updated every time
	// an assistant message arrives with usage data. Surfaced in snapshot() so
	// the Web UI can restore the context bar percentage on resume / page
	// reload / session switch, rather than flashing 0% until the next turn.
	PeakContextTokens int64 `json:"peak_context_tokens,omitempty"`

	// Backend identifies which Backend implementation drives this session.
	// "cc" (default) for claudeBackend / Claude Code stream-json subprocess;
	// "codex" for codexBackend / OpenAI codex app-server JSON-RPC. Surfaced
	// in snapshot() so the Web UI can render a backend badge per session.
	Backend BackendKind `json:"backend,omitempty"`

	// process is the active Backend driving this session. Today the only
	// concrete type is *claudeBackend (CC stream-json subprocess); Round 3
	// adds *codexBackend (JSON-RPC app-server). The field name is left as
	// "process" for diff continuity with the pre-Backend codebase — every
	// existing call site (sess.process.alive(), sess.process.sendMessage()
	// …) keeps compiling because the lowercase methods on Backend match
	// claudeBackend's existing method set.
	process     Backend
	broadcaster *sseBroadcaster
	bridgeDone  chan struct{} // closed when the current bridgeStdout goroutine exits
	promptFile  string        // temp file for soul prompt
	mcpConfig   string        // remembered for reload
	hub         *wsHub        // for WS notifications on status change
	waiters     []chan string // notified when status becomes idle/stopped/error

	// pendingAUQ tracks in-flight AskUserQuestion permission requests that the
	// Web UI is rendering. Keyed by the control_request.request_id. The claude
	// subprocess blocks waiting for a control_response (with the user's answers
	// in updatedInput) until the /answer-question endpoint fires.
	pendingAUQMu sync.Mutex
	pendingAUQ   map[string]*pendingAUQEntry

	// tasks tracks background tool invocations (Bash run_in_background, Monitor,
	// async Agent) so the Web UI's Tasks panel can render their state and
	// survive page refresh. Initialized in newServerSession.
	tasks *taskTracker

	// Model fallback for non-interactive modes (heartbeat/cron/evolve):
	// when process exits with rate_limit, retry with next fallback model.
	fallbackModels []string        // remaining models to try
	taskMessage    string          // original task message for retry
	sessionMgr     *sessionManager // back-reference for fallback retry

	// ── Phase D — Runtime Soul Patch Protocol ──
	//
	// LoadedFragments is the cumulative set of soul fragment file paths whose
	// content has already been absorbed into this session's persona — either
	// emitted into the initial system prompt at session creation, or appended
	// via a soul-patch block on a later user turn. Cumulative semantics:
	// once a fragment is loaded it stays loaded ("only add, never remove").
	// Used as the diff anchor for per-message lazy injection.
	LoadedFragments []string `json:"loaded_fragments,omitempty"`

	// PendingPatches is the queue of `<<soul-patch>>...<</soul-patch>>` blocks
	// (already wrapped, ready to ship) that the server detected a need for but
	// could not deliver yet because the user hasn't sent a message. Per protocol,
	// patch blocks must always ride along with a real user turn — so we attach
	// them to the next inbound message instead of pushing them standalone.
	PendingPatches []string `json:"pending_patches,omitempty"`

	// CurrentSoulMode mirrors the last detectSoulMode() result for this session.
	// Audit / debugging only — the lazy injector recomputes from every message.
	CurrentSoulMode string `json:"current_soul_mode,omitempty"`

	mu sync.Mutex
}

// pendingAUQEntry remembers enough about an in-flight can_use_tool
// control_request to answer it later with the user's choices. Covers two
// kinds:
//   - Kind="auq": an AskUserQuestion tool call. The answer map is merged
//     into the tool's updatedInput (legacy behavior).
//   - Kind="permission": any other tool's permission prompt (Write, Edit,
//     …). We synthesize a Yes/No AUQ payload for the Web UI; on allow we
//     pass the original Input back unchanged; on deny we send behavior=deny.
type pendingAUQEntry struct {
	RequestID string          // control_request.request_id from claude
	ToolUseID string          // tool_use_id from the can_use_tool payload (for UI dedupe)
	Input     json.RawMessage // original input — echoed back in updatedInput (AUQ merges answers; permission passes through)
	CreatedAt time.Time
	Kind      string // "auq" or "permission"
	ToolName  string // tool_name from the request; used by permission path
}

// recordPendingAUQ stores a pending AskUserQuestion by request_id.
func (s *serverSession) recordPendingAUQ(entry *pendingAUQEntry) {
	s.pendingAUQMu.Lock()
	defer s.pendingAUQMu.Unlock()
	if s.pendingAUQ == nil {
		s.pendingAUQ = make(map[string]*pendingAUQEntry)
	}
	s.pendingAUQ[entry.RequestID] = entry
}

// takePendingAUQ atomically removes and returns a pending AUQ entry.
// Returns nil if no entry exists for the given request_id.
func (s *serverSession) takePendingAUQ(requestID string) *pendingAUQEntry {
	s.pendingAUQMu.Lock()
	defer s.pendingAUQMu.Unlock()
	entry, ok := s.pendingAUQ[requestID]
	if !ok {
		return nil
	}
	delete(s.pendingAUQ, requestID)
	return entry
}

// snapshotPendingAUQ returns a JSON-safe copy of all pending AUQ entries.
//
// For Kind="permission" entries, the stored Input is the *original* tool
// input (e.g. {"file_path": "...", "content": "..."}), not the synthetic
// {"questions": [...]} payload that was broadcast live. The Web UI's
// loadSessionState path expects an AUQ-shaped input it can render directly,
// so we re-synthesize the Yes/No questions here. Without this, refreshing
// the page (or switching sessions) while a permission prompt is pending
// leaves the subprocess blocked with no UI to approve/deny it.
func (s *serverSession) snapshotPendingAUQ() []map[string]any {
	s.pendingAUQMu.Lock()
	defer s.pendingAUQMu.Unlock()
	out := make([]map[string]any, 0, len(s.pendingAUQ))
	for _, e := range s.pendingAUQ {
		var input any = e.Input
		if e.Kind == "permission" {
			input = synthesizePermissionAUQInput(e.ToolName, e.Input)
		}
		out = append(out, map[string]any{
			"request_id":  e.RequestID,
			"tool_use_id": e.ToolUseID,
			"input":       input,
			"created_at":  e.CreatedAt.Format(time.RFC3339),
			"kind":        e.Kind,
			"tool_name":   e.ToolName,
		})
	}
	return out
}

// synthesizePermissionAUQInput builds the Yes/No questions payload the Web UI
// expects for a permission prompt. Kept in sync with the live broadcast in
// makeOnCanUseTool (server_session.go ~line 854) so refresh-time rehydration
// renders an identical card.
func synthesizePermissionAUQInput(toolName string, rawInput json.RawMessage) map[string]any {
	return map[string]any{
		"questions": []map[string]any{{
			"question": fmt.Sprintf("Do you want to let Claude run %s?", toolName),
			"options": []map[string]any{
				{"label": "Yes, this time", "description": permissionInputPreview(rawInput)},
				{"label": "No, cancel", "description": "Claude will be told you declined this tool call."},
			},
			"multiSelect": false,
		}},
	}
}

// dismissAllPendingAUQ atomically clears all pending AUQ entries and returns them.
// Used in two scenarios:
//   - reason="user_message": user sent a new message instead of answering. We
//     reply behavior=deny so claude reads the new message instead of blocking.
//   - reason="session_dead": the claude subprocess exited; the control_request
//     is dead with it. We only broadcast to the UI; do NOT write stdin.
func (s *serverSession) dismissAllPendingAUQ(reason string) []*pendingAUQEntry {
	s.pendingAUQMu.Lock()
	pending := make([]*pendingAUQEntry, 0, len(s.pendingAUQ))
	for _, e := range s.pendingAUQ {
		pending = append(pending, e)
	}
	s.pendingAUQ = nil
	s.pendingAUQMu.Unlock()

	if len(pending) == 0 {
		return nil
	}

	proc := s.process
	for _, e := range pending {
		// Only attempt control_response if the subprocess is still alive.
		if reason == "user_message" && proc != nil && proc.alive() {
			decision := map[string]any{
				"behavior": "deny",
				"message":  "User sent a new message instead of answering. Read it and proceed.",
			}
			_ = proc.sendPermissionDecision(e.RequestID, decision)
		}
		// Always broadcast so the Web UI can mark the card grey.
		evt, _ := json.Marshal(map[string]any{
			"request_id":  e.RequestID,
			"tool_use_id": e.ToolUseID,
			"reason":      reason,
		})
		s.broadcaster.broadcast(sseEvent{Event: "ask_user_question_dismissed", Data: evt})
	}
	return pending
}

// touch updates LastActive timestamp.
func (s *serverSession) touch() {
	s.mu.Lock()
	s.LastActive = time.Now()
	s.mu.Unlock()
}

// updatePeakContext raises PeakContextTokens to total if total is larger.
// Notifies the WS hub when the value changes so sidebars stay in sync.
// Safe to call concurrently.
func (s *serverSession) updatePeakContext(total int64) {
	if total <= 0 {
		return
	}
	s.mu.Lock()
	changed := total > s.PeakContextTokens
	if changed {
		s.PeakContextTokens = total
	}
	hub := s.hub
	s.mu.Unlock()
	if changed && hub != nil {
		hub.notifySessions()
	}
}

// setTodos stores the latest TodoWrite state and broadcasts session list update.
func (s *serverSession) setTodos(todos json.RawMessage) {
	s.mu.Lock()
	s.Todos = todos
	hub := s.hub
	s.mu.Unlock()
	if hub != nil {
		hub.notifySessions()
	}
}

// setStatus atomically updates session status and notifies WS clients.
func (s *serverSession) setStatus(status string) {
	s.mu.Lock()
	changed := s.Status != status
	s.Status = status
	id := s.ID
	hub := s.hub
	// Notify waiters when session reaches a terminal/current state
	var waitersToNotify []chan string
	if changed && (status == "idle" || status == "stopped" || status == "error") {
		waitersToNotify = s.waiters
		s.waiters = nil
	}
	s.mu.Unlock()
	// Fire waiter notifications outside the lock
	for _, ch := range waitersToNotify {
		ch <- status
	}
	if changed {
		// Persist proxy-aggregated cost when session ends
		if status == "stopped" || status == "error" {
			go persistSessionCost(id)
		}
		if hub != nil {
			hub.notifySessions()
		}
	}
}

// awaitBackendReady blocks until the backend can accept a user turn and
// reports whether it is safe to send one. Every "inject the first message
// after init" path should route through here instead of calling
// process.waitInit directly.
//
// Three outcomes:
//
//   - init completed              → ready (true)
//   - init timed out, still alive → ready (true), with a loud log line. A slow
//     provider is not a reason to drop the user's first message.
//   - backend not alive after init → NOT ready (false), and the session is
//     forced into the terminal "error" status. Covers both a dead process and
//     a handshake the backend deliberately aborted (e.g. codex rejecting the
//     thread/start params).
//
// The third case is the one that used to be swallowed. waitInit returns false
// both for "too slow" and for "already dead", callers treated the two the
// same, and the message went to a dead process: the write failed, the
// goroutine logged and returned, and the session sat there with a
// non-terminal status until it was reported as a completed run. A backend
// whose handshake fails (bad protocol field, auth error, missing binary)
// therefore produced a dispatched task that did no work and reported success.
func (s *serverSession) awaitBackendReady(timeout time.Duration) bool {
	proc := s.process
	if proc == nil {
		return false
	}
	if proc.waitInit(timeout) {
		return true
	}
	if !proc.alive() {
		// Either the process died, or it aborted the handshake and deliberately
		// marked itself done (e.g. codex rejecting thread/start params). Both
		// mean no work can happen; the backend logged the cause at init time.
		fmt.Fprintf(os.Stderr, "[%s] server: backend for %s failed to initialize; not sending initial message (see log above for the cause)\n", appName, shortID(s.ID))
		s.setStatus("error")
		// setStatus is in-memory only. Persist too: rehydration restores rows
		// with status IN ('active','suspended'), so a failed session left
		// "active" in the DB would be resurrected on the next server restart.
		updateSessionStatus(s.ID, "error")
		return false
	}
	fmt.Fprintf(os.Stderr, "[%s] server: init timeout for %s, sending initial message anyway\n", appName, shortID(s.ID))
	return true
}

// snapshot returns a JSON-safe copy of session state.
// NOTE: To avoid deadlocks, DB queries and broadcaster calls are done OUTSIDE
// sess.mu. Lock ordering: sess.mu must never be held when acquiring broadcaster.mu
// (broadcast() holds broadcaster.mu → calls hub → calls listSessions → snapshot).
func (s *serverSession) snapshot() map[string]any {
	// 1. Read broadcaster metrics BEFORE acquiring sess.mu (avoids sess.mu → broadcaster.mu inversion)
	subCount := s.broadcaster.count()
	lastEvent := s.broadcaster.lastEventAt()
	idleSecs := s.broadcaster.idleSeconds()

	// 2. Copy session fields under lock
	s.mu.Lock()
	id := s.ID
	galID := s.GalID
	tags := s.Tags
	if tags == nil {
		tags = []string{}
	}
	var todosSnap json.RawMessage
	if len(s.Todos) > 0 {
		todosSnap = make(json.RawMessage, len(s.Todos))
		copy(todosSnap, s.Todos)
	}
	spawnedBy := s.SpawnedBy
	peakCtx := s.PeakContextTokens
	backendKind := s.Backend
	if backendKind == "" {
		backendKind = BackendCC
	}
	snap := map[string]any{
		"id":                  id,
		"name":                s.Name,
		"project":             s.Project,
		"model":               s.Model,
		"backend":             string(backendKind),
		"status":              s.Status,
		"category":            s.Category,
		"tags":                tags,
		"created_at":          s.CreatedAt.Format(time.RFC3339),
		"last_active":         s.LastActive.Format(time.RFC3339),
		"total_cost_usd":      s.TotalCost,
		"num_turns":           s.NumTurns,
		"claude_session_id":   s.ClaudeSID,
		"resumed_from":        s.ResumedFrom,
		"stream_url":          s.StreamURL,
		"agent":               "main",
		"soul_enabled":        s.SoulEnabled,
		"chrome_enabled":      s.ChromeEnabled,
		"effort":              s.Effort,
		"replace_soul":        s.ReplaceSoul,
		"mode":                flagsToMode(s.SoulEnabled, s.ReplaceSoul),
		"first_msg":           s.FirstMsg,
		"spawned_by":          spawnedBy,
		"peak_context_tokens": peakCtx,
	}
	if todosSnap != nil {
		snap["todos"] = todosSnap
	}
	s.mu.Unlock()

	// 3. DB queries and broadcaster values OUTSIDE lock (no deadlock risk)
	if galID == "" {
		galID = getGalID(id)
	}
	snap["gal_id"] = galID
	snap["proxy_cost_usd"] = getSessionProxyCost(id)
	snap["subscribers"] = subCount
	snap["last_event"] = lastEvent
	snap["idle_seconds"] = idleSecs
	snap["participants"] = getParticipants(id)

	return snap
}

// ── Session Lifecycle Helpers ──

// makeOnInit returns the onInit callback for bridgeStdout.
// source: "server-create", "server-resume" → full init (record agent, sync CC name).
// source: "" → reload init (only sync ClaudeSID/Model, notify hub).
func makeOnInit(sess *serverSession, source string) func(json.RawMessage) {
	return func(raw json.RawMessage) {
		var init InitMessage
		if json.Unmarshal(raw, &init) == nil && init.SessionID != "" {
			sess.mu.Lock()
			sess.ClaudeSID = init.SessionID
			if init.Model != "" {
				before := sess.Model
				sess.Model = mergeInitModel(sess.Model, init.Model)
				fmt.Fprintf(os.Stderr, "[%s] server: onInit model: before=%q init=%q after=%q (session %s)\n",
					appName, before, init.Model, sess.Model, shortID(sess.ID))
			}
			proc := sess.process // capture under lock
			hub := sess.hub      // read under lock to avoid race
			sess.mu.Unlock()

			// Mirror the CC session id onto the backend so info() can
			// surface it without reaching back into serverSession state.
			if cb, ok := proc.(*claudeBackend); ok {
				cb.setClaudeSessionID(init.SessionID)
			}

			if source != "" {
				// Full init: record mappings and sync CC name
				setClaudeSessionID(sess.ID, init.SessionID)
				recordSessionAgent(init.SessionID, "main", appName, source)
				if ccName := readClaudeSessionName(init.SessionID); ccName != "" {
					sess.mu.Lock()
					if sess.Name == "" || strings.HasPrefix(sess.Name, "session-") || strings.HasPrefix(sess.Name, "resume-") {
						sess.Name = ccName
					}
					sess.mu.Unlock()
					ensureServerSession(sess.ID, ccName)
				}
			}
			if hub != nil {
				hub.notifySessions()
			}
		}
	}
}

// makeOnResult returns the onResult callback for bridgeStdout.
// fullSync: true for create/resume (CC name sync + auto-rename tracking).
// fullSync: false for reload (cost/turns only).
func makeOnResult(sess *serverSession, fullSync bool) func(json.RawMessage) {
	return func(raw json.RawMessage) {
		var result ResultMessage
		if json.Unmarshal(raw, &result) == nil {
			sess.mu.Lock()
			sess.TotalCost += result.TotalCostUSD
			sess.NumTurns += result.NumTurns
			if result.SessionID != "" {
				sess.ClaudeSID = result.SessionID
			}
			newStatus := "idle"
			if result.Subtype == "error" {
				newStatus = "error"
				// Detect 429/rate-limit in result text for ephemeral sessions.
				// Claude Code handles 429 internally and may exit cleanly (code 0),
				// so we must detect it here in stdout and flag the process for fallback.
				if isEphemeralCategory(sess.Category) && len(sess.fallbackModels) > 0 && sess.process != nil {
					lower := strings.ToLower(result.Result)
					if strings.Contains(lower, "429") || strings.Contains(lower, "rate limit") ||
						strings.Contains(lower, "too many requests") || strings.Contains(lower, "quota exceeded") {
						proc := sess.process
						sess.mu.Unlock()
						// Route through Backend methods so this code path
						// works for any Backend (CC today, codex Round 3).
						// The atomic flag and SIGKILL are CC implementation
						// details, but markRateLimited / killProcess are
						// part of the interface contract.
						proc.markRateLimited()
						fmt.Fprintf(os.Stderr, "[%s] server: detected 429/rate-limit in result message, killing process for fallback\n", appName)
						proc.killProcess()
						return
					}
				}
			}
			sess.mu.Unlock()
			sess.setStatus(newStatus)

			if fullSync {
				// Sync session name from Claude Code metadata
				sess.mu.Lock()
				claudeSID := sess.ClaudeSID
				currentName := sess.Name
				hub := sess.hub // read under lock
				sess.mu.Unlock()
				if claudeSID != "" && !isManuallyRenamed(sess.ID) {
					if ccName := readClaudeSessionName(claudeSID); ccName != "" && ccName != currentName {
						sess.mu.Lock()
						sess.Name = ccName
						sess.mu.Unlock()
						markAutoNamed(sess.ID, ccName)
						if hub != nil {
							hub.notifySessions()
						}
					}
				}

				// Track user turns for auto-rename
				if result.NumTurns > 0 {
					_, _ = incrementUserTurns(sess.ID)
					// Auto-rename disabled 2026-04-16: removes the only automated
					// Anthropic API call (haiku pool) when user runs on GLM/MiniMax
					// defaults. Manual rename via POST /api/sessions/{id}/auto-rename
					// still works if the user explicitly wants it.
					// if !renamed && turns > 0 && turns%5 == 0 && !isAutoNamed(sess.ID) {
					//     go tryAutoRename(sess)
					// }
				}
			}
		}
		sess.touch()
	}
}

// watchExit monitors a CC backend process exit and updates session status.
// For ephemeral sessions (heartbeat/cron/evolve), if the exit is classified
// as rate_limit and fallback models are available, automatically retries
// with the next fallback model.
//
// Takes *claudeBackend (not Backend) because rate-limit classification reads
// CC-specific atomic / stderr fields directly. Round 3's codexBackend gets
// its own watchExit equivalent that classifies JSON-RPC error codes.
func watchExit(proc *claudeBackend, sess *serverSession) {
	go func() {
		<-proc.done
		sess.mu.Lock()
		alreadyStopped := sess.Status == "stopped"
		category := sess.Category
		fallbacks := append([]string(nil), sess.fallbackModels...)
		taskMsg := sess.taskMessage
		sm := sess.sessionMgr
		model := sess.Model
		claudeSID := sess.ClaudeSID
		sess.mu.Unlock()
		if alreadyStopped {
			return
		}
		// Determine if this was a rate_limit exit (either from exit code classification
		// or from real-time stderr detection via the rateLimited flag).
		isRateLimit := false
		if isEphemeralCategory(category) && len(fallbacks) > 0 && sm != nil {
			if proc.rateLimited.Load() {
				isRateLimit = true
			} else if proc.exitCode != 0 {
				event := classifyExitEvent(proc.exitCode, "", proc.stderrTail.String())
				isRateLimit = (event == "rate_limit")
			}
		}

		if isRateLimit {
			nextModel := fallbacks[0]
			remaining := fallbacks[1:]
			fmt.Fprintf(os.Stderr, "[%s] server: %s session %s hit rate_limit on %s, falling back to %s\n",
				appName, category, shortID(sess.ID), model, nextModel)
			// Fire-and-forget retry with next fallback model
			go retryWithFallbackModel(sm, sess, nextModel, remaining, taskMsg, claudeSID)
			sess.setStatus("error")
			return
		}

		// Log exit diagnostics so we can see why CC died (esp. for resume flows
		// that fail with init timeout / immediate reap).
		stderrTail := proc.stderrTail.String()
		if stderrTail != "" {
			// Trim to last 400 chars to avoid log flooding.
			if len(stderrTail) > 400 {
				stderrTail = "…" + stderrTail[len(stderrTail)-400:]
			}
		}
		fmt.Fprintf(os.Stderr, "[%s] server: session %s process exited code=%d stderr=%q\n",
			appName, shortID(sess.ID), proc.exitCode, stderrTail)

		if proc.exitCode != 0 {
			sess.setStatus("error")
		} else {
			sess.setStatus("stopped")
		}
	}()
}

// retryWithFallbackModel creates a new ephemeral session with the next fallback model
// after the current one failed with rate_limit.
func retryWithFallbackModel(sm *sessionManager, oldSess *serverSession, model string, remaining []string, taskMsg string, claudeSID string) {
	oldSess.mu.Lock()
	category := oldSess.Category
	project := oldSess.Project
	soul := oldSess.SoulEnabled
	replaceSoul := oldSess.ReplaceSoul
	oldModel := oldSess.Model
	oldSess.mu.Unlock()

	sessName := fmt.Sprintf("%s-fallback-%s", category, time.Now().Format("0102-150405"))
	fmt.Fprintf(os.Stderr, "[%s] server: creating fallback session %s with model %s (%d remaining fallbacks)\n",
		appName, sessName, model, len(remaining))

	// Resume the original Claude session to preserve conversation context.
	// For heartbeat, prefer the soul session (long-lived); for cron/evolve,
	// resume the specific Claude session that hit rate_limit.
	var resumeID string
	if category == CategoryHeartbeat {
		resumeID = getActiveSoulSession(agentName)
	}
	if resumeID == "" && claudeSID != "" {
		resumeID = claudeSID
		fmt.Fprintf(os.Stderr, "[%s] server: fallback resuming original Claude session %s\n",
			appName, shortID(claudeSID))
	}

	sess, err := sm.createSessionWithOpts(sessionCreateOpts{
		Name:        sessName,
		Project:     project,
		Model:       model,
		Soul:        soul,
		Category:    category,
		Tags:        []string{"auto", "fallback"},
		ReplaceSoul: replaceSoul,
		ResumeID:    resumeID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] server: fallback session creation failed: %v\n", appName, err)
		// Continue consuming remaining fallback chain instead of giving up
		if len(remaining) > 0 {
			next := remaining[0]
			fmt.Fprintf(os.Stderr, "[%s] server: skipping %s, trying next fallback: %s\n", appName, model, next)
			go retryWithFallbackModel(sm, oldSess, next, remaining[1:], taskMsg, claudeSID)
		} else {
			go sendTelegram(fmt.Sprintf("❌ %s all fallback models exhausted (last: %s): %v", category, model, err))
		}
		return
	}

	// Set remaining fallbacks on the new session
	sess.mu.Lock()
	sess.fallbackModels = remaining
	sess.taskMessage = taskMsg
	sess.sessionMgr = sm
	sess.mu.Unlock()

	// Send the original task message — only if we didn't resume an existing
	// Claude session (which already has the task in its conversation history).
	// Sending it again on a resumed session would cause duplicate execution.
	if taskMsg != "" && resumeID != "" {
		fmt.Fprintf(os.Stderr, "[%s] server: fallback resumed session %s, skipping taskMsg re-send (already in conversation history)\n",
			appName, shortID(resumeID))
	}
	if taskMsg != "" && resumeID == "" {
		go func() {
			if !sess.awaitBackendReady(30 * time.Second) {
				return
			}
			if err := sess.process.sendMessage(taskMsg); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] server: fallback session failed to send task: %v\n", appName, err)
			}
		}()
	}

	// Notify via Telegram
	notifyMsg := fmt.Sprintf("⚠️ %s model rate-limited, retrying with fallback: %s", oldModel, model)
	go sendTelegram(notifyMsg)
}

// waitBridgeDone waits for the current bridgeStdout goroutine to exit (max 10s).
// Must be called AFTER shutting down the old process but BEFORE starting a new bridge.
func (s *serverSession) waitBridgeDone() {
	s.mu.Lock()
	done := s.bridgeDone
	s.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		fmt.Fprintf(os.Stderr, "[%s] server: bridge drain timeout for %s\n", appName, shortID(s.ID))
	}
}

// drainStderr reads stderr to prevent pipe deadlock, capturing the last 4KB
// in proc.stderrTail for exit event classification (e.g. rate_limit
// detection). For ephemeral sessions with fallback models, it also detects
// 429/rate-limit errors in real time and kills the process to trigger
// model fallback via watchExit.
//
// CC-specific: takes *claudeBackend directly because it pulls bytes off the
// CC stderr pipe and writes the captured tail into a CC field. Round 3's
// codex backend will have its own drain equivalent.
func drainStderr(proc *claudeBackend, sess *serverSession) {
	// Determine if we should monitor for rate-limit errors
	sess.mu.Lock()
	shouldMonitor := isEphemeralCategory(sess.Category) && len(sess.fallbackModels) > 0
	sess.mu.Unlock()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := proc.stderr.Read(buf)
			if n > 0 {
				proc.stderrTail.Write(buf[:n])

				// Real-time 429 detection for ephemeral sessions.
				// Signature list lives on looksLikeRateLimit (backend.go)
				// so codex's drainStderr stays in sync automatically.
				if shouldMonitor && !proc.rateLimited.Load() {
					if looksLikeRateLimit(strings.ToLower(string(buf[:n]))) {
						proc.rateLimited.Store(true)
						fmt.Fprintf(os.Stderr, "[%s] server: detected 429/rate-limit in stderr, killing process for fallback\n", appName)
						proc.cmd.Process.Kill()
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
}

// attachProcessBridge sets up stdout→SSE bridge, exit watcher, and stderr
// drain for a freshly-spawned CC backend.
//
// CC-specific: takes *claudeBackend rather than Backend because the SSE
// bridge consumes the stream-json line stream off the CC stdout pipe.
// Round 3's codexBackend will have its own attachCodexBridge that consumes
// JSON-RPC notifications from the codex client and emits SSE events with
// the same schema (so the Web UI stays unchanged).
//
// source: "server-create"/"server-resume" for full init, "" for reload.
// fullSync: true for create/resume (CC name sync + auto-rename), false for
// reload.
func attachProcessBridge(proc *claudeBackend, sess *serverSession, source string, fullSync bool) {
	doneCh := make(chan struct{})
	go func() {
		bridgeStdout(proc, sess.broadcaster, makeOnInit(sess, source), makeOnResult(sess, fullSync), func(todos json.RawMessage) {
			sess.setTodos(todos)
		}, makeOnMemoryAudit(sess), makeOnCanUseTool(sess), makeOnTask(sess), func(total int64) {
			sess.updatePeakContext(total)
		}, makeOnSegmentComplete(sess))
		// Process exited: invalidate pending AUQ (the control_request is gone
		// with the subprocess) and mark in-flight tasks cancelled so the UI
		// stops showing them as still running.
		sess.dismissAllPendingAUQ("session_dead")
		if cancelled := sess.tasks.markAllRunningAsCancelled(); len(cancelled) > 0 {
			for _, t := range cancelled {
				if data, err := json.Marshal(t); err == nil {
					sess.broadcaster.broadcast(sseEvent{Event: "task_event", Data: data})
				}
			}
		}
		close(doneCh)
		// Clear bridgeDone if it still points to this generation's channel,
		// so subsequent waitBridgeDone() won't return immediately on a stale close.
		sess.mu.Lock()
		if sess.bridgeDone == doneCh {
			sess.bridgeDone = nil
		}
		sess.mu.Unlock()
	}()
	sess.mu.Lock()
	sess.bridgeDone = doneCh
	sess.mu.Unlock()
	watchExit(proc, sess)
	drainStderr(proc, sess)
}

// makeOnSegmentComplete returns a callback for PostAssistantMsg content hook.
// Called when a mid-turn assistant text segment completes (tool_use follows).
// Opens its own DB handle so evaluation doesn't block the main stream.
func makeOnSegmentComplete(sess *serverSession) func(text string) {
	db := openAuditDB()
	if db != nil {
		ensureContentHookTable(db)
	}
	return func(text string) {
		sess.mu.Lock()
		sessionID := sess.ClaudeSID // CC session ID — matches what tool-hook binary sees
		sess.mu.Unlock()
		if sessionID == "" {
			return
		}
		// Run evaluation async to not block the stream handler
		go evalPostAssistantMsg(db, sessionID, text)
	}
}

// makeOnMemoryAudit returns a callback that logs memory operations to the audit DB.
func makeOnMemoryAudit(sess *serverSession) func(*memoryAuditEntry) {
	db := openAuditDB()
	return func(entry *memoryAuditEntry) {
		sess.mu.Lock()
		entry.SessionID = sess.ID
		entry.SessionName = sess.Name
		sess.mu.Unlock()
		go logMemoryAudit(db, *entry)
	}
}

// makeOnTask returns a callback that feeds tool_use / tool_result events to
// the session-scoped task tracker. For each created or updated task, an SSE
// event "task_event" is broadcast so the Web UI's Tasks panel can update in
// real time.
func makeOnTask(sess *serverSession) func(event string, raw json.RawMessage) {
	return func(event string, raw json.RawMessage) {
		var changed []*runningTask
		switch event {
		case "tool_use":
			if t := sess.tasks.trackToolUse(raw); t != nil {
				changed = append(changed, t)
			}
		case "tool_result":
			changed = sess.tasks.trackToolResult(raw)
		}
		for _, t := range changed {
			if data, err := json.Marshal(t); err == nil {
				sess.broadcaster.broadcast(sseEvent{Event: "task_event", Data: data})
			}
		}
	}
}

// makeOnCanUseTool returns a callback that claims AskUserQuestion
// can_use_tool control_requests for Web UI handling. For other tools that
// unexpectedly reach this path (shouldn't happen in bypassPermissions mode
// but belt-and-suspenders), returns false so bridgeStdout auto-allows.
//
// Claims AskUserQuestion by: (1) recording a pendingAUQEntry keyed by
// request_id, (2) broadcasting an ask_user_question SSE event with the
// input so the Web UI can render the choice panel. The subprocess stays
// blocked until /api/sessions/{id}/answer-question fires and
// sendPermissionDecision unblocks it.
func makeOnCanUseTool(sess *serverSession) func(json.RawMessage) bool {
	return func(raw json.RawMessage) bool {
		var payload struct {
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype   string          `json:"subtype"`
				ToolName  string          `json:"tool_name"`
				ToolUseID string          `json:"tool_use_id"`
				Input     json.RawMessage `json:"input"`
			} `json:"request"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return false
		}

		// AskUserQuestion: the tool already carries the questions/options
		// schema, so forward the raw input to the Web UI unchanged.
		if payload.Request.ToolName == "AskUserQuestion" {
			entry := &pendingAUQEntry{
				RequestID: payload.RequestID,
				ToolUseID: payload.Request.ToolUseID,
				Input:     append(json.RawMessage(nil), payload.Request.Input...),
				CreatedAt: time.Now(),
				Kind:      "auq",
				ToolName:  payload.Request.ToolName,
			}
			sess.recordPendingAUQ(entry)

			evt, _ := json.Marshal(map[string]any{
				"request_id":  payload.RequestID,
				"tool_use_id": payload.Request.ToolUseID,
				"input":       payload.Request.Input,
				"kind":        "auq",
				"tool_name":   payload.Request.ToolName,
			})
			sess.broadcaster.broadcast(sseEvent{Event: "ask_user_question", Data: evt})
			return true
		}

		// ExitPlanMode: plan approval dialog. Claude Code injects plan
		// content into the tool input via normalizeToolInput. Surface the
		// full plan to the Web UI so the user can review, optionally add
		// feedback, and approve/reject — instead of the generic Yes/No.
		if payload.Request.ToolName == "ExitPlanMode" {
			entry := &pendingAUQEntry{
				RequestID: payload.RequestID,
				ToolUseID: payload.Request.ToolUseID,
				Input:     append(json.RawMessage(nil), payload.Request.Input...),
				CreatedAt: time.Now(),
				Kind:      "plan_approval",
				ToolName:  "ExitPlanMode",
			}
			sess.recordPendingAUQ(entry)

			// Extract plan/planFilePath/allowedPrompts from the normalized input.
			var pi struct {
				Plan           string          `json:"plan"`
				PlanFilePath   string          `json:"planFilePath"`
				AllowedPrompts json.RawMessage `json:"allowedPrompts"`
			}
			_ = json.Unmarshal(payload.Request.Input, &pi)

			evt, _ := json.Marshal(map[string]any{
				"request_id":      payload.RequestID,
				"tool_use_id":     payload.Request.ToolUseID,
				"plan":            pi.Plan,
				"plan_file_path":  pi.PlanFilePath,
				"allowed_prompts": pi.AllowedPrompts,
			})
			sess.broadcaster.broadcast(sseEvent{Event: "plan_approval", Data: evt})
			return true
		}

		// Non-AUQ permission prompt (Write, Edit, Bash, …). Claude Code
		// normally runs with --dangerously-skip-permissions, so these only
		// surface for tools the harness can't silently approve (e.g. skill
		// bootstrap creating SKILL.md). Mirror the TUI behavior: surface
		// Yes/No to the user instead of auto-allowing in the background.
		entry := &pendingAUQEntry{
			RequestID: payload.RequestID,
			ToolUseID: payload.Request.ToolUseID,
			Input:     append(json.RawMessage(nil), payload.Request.Input...),
			CreatedAt: time.Now(),
			Kind:      "permission",
			ToolName:  payload.Request.ToolName,
		}
		sess.recordPendingAUQ(entry)

		synthetic, _ := json.Marshal(map[string]any{
			"request_id":  payload.RequestID,
			"tool_use_id": payload.Request.ToolUseID,
			"kind":        "permission",
			"tool_name":   payload.Request.ToolName,
			"input":       synthesizePermissionAUQInput(payload.Request.ToolName, payload.Request.Input),
		})
		sess.broadcaster.broadcast(sseEvent{Event: "ask_user_question", Data: synthetic})
		return true
	}
}

// permissionInputPreview renders tool_input as a short, human-readable
// preview for the Yes option's description. Truncates to 240 chars so
// the AUQ card stays compact.
func permissionInputPreview(input json.RawMessage) string {
	const maxLen = 240
	if len(input) == 0 {
		return "(no arguments)"
	}
	// Try to pretty-print known key=value shapes; fall back to raw JSON.
	var obj map[string]any
	if err := json.Unmarshal(input, &obj); err == nil && len(obj) > 0 {
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for i, k := range keys {
			if i > 0 {
				b.WriteString("  ")
			}
			val, _ := json.Marshal(obj[k])
			fmt.Fprintf(&b, "%s=%s", k, string(val))
			if b.Len() >= maxLen {
				break
			}
		}
		s := b.String()
		if len(s) > maxLen {
			s = s[:maxLen] + "…"
		}
		return s
	}
	s := string(input)
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}
	return s
}

// ── Session Manager ──

// sessionManager manages concurrent sessions with TTL.
type sessionManager struct {
	sessions    map[string]*serverSession
	maxSessions int
	idleTimeout time.Duration
	maxLifetime time.Duration
	mu          sync.RWMutex
	stopReaper  chan struct{}
	reaperDone  sync.WaitGroup // waited by shutdownAll to ensure reaper has exited
	hub         *wsHub         // WebSocket hub for real-time notifications
	resuming    sync.Map       // session_id → true; prevents concurrent resume spawning orphan processes
}

// newSessionManager creates a session manager and starts the TTL reaper.
func newSessionManager(maxSessions int, idleTimeout, maxLifetime time.Duration) *sessionManager {
	sm := &sessionManager{
		sessions:    make(map[string]*serverSession),
		maxSessions: maxSessions,
		idleTimeout: idleTimeout,
		maxLifetime: maxLifetime,
		stopReaper:  make(chan struct{}),
	}
	sm.reaperDone.Add(1)
	go sm.reaper()
	return sm
}

// sessionCreateOpts holds options for creating a new session.
type sessionCreateOpts struct {
	Name        string
	Project     string
	Model       string
	Soul        bool
	MCP         string
	GalID       string
	Category    string   // interactive, heartbeat, cron, evolve, telegram (default: interactive)
	Tags        []string // freeform labels
	EnvOverride string   // if set, replaces the environment section in BOOT.md
	ReplaceSoul bool     // 本我模式 — use --system-prompt-file instead of --append-system-prompt-file
	ResumeID    string   // Claude Code session ID to resume (adds --resume <id>)
	SpawnedBy   string   // parent session ID that spawned this one
	// InitialMessage feeds intimate-trigger regex matching at prompt build
	// time. Stored separately from the broadcast/sendMessage path because
	// we need it BEFORE the backend spawns, while the broadcast happens
	// AFTER the session is up.
	InitialMessage string
	// Backend selects which Backend implementation drives the session.
	// Empty defaults through resolveBackendKind: explicit > model auto-route
	// > BackendCC.
	Backend BackendKind
}

// createSession spawns a new Claude Code session.
func (sm *sessionManager) createSession(name, project, model string, soulEnabled bool, mcpConfig string, galID string) (*serverSession, error) {
	return sm.createSessionWithOpts(sessionCreateOpts{
		Name:     name,
		Project:  project,
		Model:    model,
		Soul:     soulEnabled,
		MCP:      mcpConfig,
		GalID:    galID,
		Category: CategoryInteractive,
	})
}

// createSessionWithOpts spawns a new Claude Code session with full options.
func (sm *sessionManager) createSessionWithOpts(opts sessionCreateOpts) (*serverSession, error) {
	// Expand ~ in project path and validate it exists
	if opts.Project != "" {
		expanded, err := expandHome(opts.Project)
		if err != nil {
			return nil, fmt.Errorf("invalid project path %q: %w", opts.Project, err)
		}
		opts.Project = expanded
		info, err := os.Stat(opts.Project)
		if err != nil {
			return nil, fmt.Errorf("project directory %q does not exist: %w", opts.Project, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("project path %q is not a directory", opts.Project)
		}
	}

	category := opts.Category
	if category == "" {
		category = CategoryInteractive
	}

	// Backend selection: explicit (from API body / spawn flag) > model
	// auto-route > BackendCC default. Resolve before fuzzy model matching so
	// backend-native names such as "gpt-5.5" or "grok-4" can pass through
	// unchanged when the caller explicitly requested codex/grok.
	backendKind := resolveBackendKind(opts.Backend, opts.Model)

	// Fuzzy resolve CC/provider model names (e.g. "glm" → "zai/glm-5.1") —
	// defensive, callers should already resolve but bare spawn via API may
	// not. Non-CC backends own their model namespace, so do not run them
	// through the provider model registry.
	if opts.Model != "" && backendKind == BackendCC {
		if resolved, err := resolveFuzzyModel(opts.Model); err == nil && resolved != "" {
			opts.Model = resolved
		} else if err != nil {
			return nil, fmt.Errorf("model %q could not be resolved: %w", opts.Model, err)
		} else {
			return nil, fmt.Errorf("model %q could not be resolved", opts.Model)
		}
	}

	// Reserve a slot atomically: check limit + register placeholder in one lock scope.
	// This prevents TOCTOU where two concurrent creates both pass the check.
	id := uuid.New().String()
	now := time.Now()

	bc := newBroadcaster()
	bc.hub = sm.hub
	bc.sessionID = id

	tags := opts.Tags
	if tags == nil {
		tags = []string{}
	}

	sess := &serverSession{
		ID:          id,
		Name:        opts.Name,
		Project:     opts.Project,
		Model:       opts.Model,
		Backend:     backendKind,
		Status:      "starting",
		Category:    category,
		Tags:        tags,
		CreatedAt:   now,
		LastActive:  now,
		SoulEnabled: opts.Soul,
		ReplaceSoul: opts.ReplaceSoul,
		StreamURL:   fmt.Sprintf("/api/sessions/%s/stream", id),
		mcpConfig:   opts.MCP,
		broadcaster: bc,
		hub:         sm.hub,
		GalID:       opts.GalID,
		SpawnedBy:   opts.SpawnedBy,
		tasks:       newTaskTracker(),
	}

	sm.mu.Lock()
	if category == CategoryInteractive && sm.maxSessions > 0 {
		interactiveCount := 0
		for _, s := range sm.sessions {
			if s.Category == CategoryInteractive {
				interactiveCount++
			}
		}
		if interactiveCount >= sm.maxSessions {
			sm.mu.Unlock()
			return nil, fmt.Errorf("max interactive sessions limit reached (%d)", sm.maxSessions)
		}
	}
	// Register immediately to hold the slot (status="starting")
	sm.sessions[id] = sess
	sm.mu.Unlock()

	// On failure, remove the placeholder
	cleanup := func() {
		sm.mu.Lock()
		delete(sm.sessions, id)
		sm.mu.Unlock()
	}

	// Build soul prompt if enabled
	var promptFile string
	if opts.Soul {
		initSessionDir()

		// Build prompt with per-session overrides (concurrency-safe).
		// Routing fields (LaunchDir/FirstMessage/SessionID) are populated so
		// detectSoulMode + audit see real per-session inputs instead of the
		// daemon's launch-time globals. opts.Project has already been
		// expandHome-d above.
		ovr := promptOverrides{
			EnvOverride:  opts.EnvOverride,
			LaunchDir:    opts.Project,
			FirstMessage: opts.InitialMessage,
			SessionID:    id,
		}
		if opts.Category == CategoryHeartbeat {
			ovr.Mode = "heartbeat"
		} else if opts.Category == CategoryCron {
			ovr.Mode = "cron"
		}
		// Load GAL save for resume if provided
		if opts.GalID != "" {
			galPath := filepath.Join(workspace, "memory", "gal", opts.GalID+".json")
			if data, err := os.ReadFile(galPath); err == nil {
				ovr.GalContext = string(data)
			}
		}

		result := buildPromptWithOverrides(ovr)
		promptFile = writePromptForSession(id, result)
		sess.promptFile = promptFile

		// Phase D — capture which soul fragments the initial prompt assembly
		// emitted so per-message lazy soul-patch injection knows what is
		// already in the persona and only patches in the *new* fragments.
		// Mode is recorded for audit / debugging only — the per-message hook
		// always recomputes from the inbound message.
		if len(result.LoadedFragments) > 0 {
			sess.LoadedFragments = append([]string(nil), result.LoadedFragments...)
		}
		if result.SoulMode != "" {
			sess.CurrentSoulMode = string(result.SoulMode)
		}
		// Defer the persistence write to *after* ensureServerSessionFull
		// (further down) inserts the row, otherwise the UPDATE no-ops.
	}

	// Spawn the chosen Backend. spawnOpts is shared across both paths so
	// the WorkDir / Model / Resume semantics stay identical; only the
	// process implementation differs.
	spawnOpts := sessionOpts{
		WorkDir:          opts.Project,
		SystemPromptFile: promptFile,
		Model:            opts.Model,
		MCPConfig:        opts.MCP,
		ReplaceSoul:      opts.ReplaceSoul,
		ResumeID:         opts.ResumeID,
		ServerSessionID:  id,
		Backend:          backendKind,
	}
	var proc Backend
	switch backendKind {
	case BackendCodex:
		cb, err := spawnCodex(spawnOpts)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("spawn codex: %w", err)
		}
		proc = cb
		recordBackendStart(BackendCodex)
	case BackendGrok:
		gb, err := spawnGrok(spawnOpts)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("spawn grok: %w", err)
		}
		proc = gb
		recordBackendStart(BackendGrok)
	case BackendKimi:
		kb, err := spawnKimi(spawnOpts)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("spawn kimi: %w", err)
		}
		proc = kb
		recordBackendStart(BackendKimi)
	case BackendAgy:
		ab, err := spawnAgy(spawnOpts)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("spawn agy: %w", err)
		}
		proc = ab
		recordBackendStart(BackendAgy)
	case BackendOpencode:
		ob, err := spawnOpencode(spawnOpts)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("spawn opencode: %w", err)
		}
		proc = ob
		recordBackendStart(BackendOpencode)
	default:
		ccb, err := spawnClaude(spawnOpts)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("spawn claude: %w", err)
		}
		proc = ccb
		recordBackendStart(BackendCC)
	}
	sess.mu.Lock()
	sess.process = proc
	sess.mu.Unlock()
	sess.setStatus("running")

	// Register in server DB with category and tags
	ensureServerSessionFull(id, opts.Name, category, opts.Tags)
	if opts.Model != "" {
		setSessionModel(id, opts.Model)
	}
	// Persist the project (cwd) so resume can relaunch CC in the same dir and
	// find the transcript. Without this, spawn sessions created outside the
	// default workspace can't be resumed — CC launches in workspace, fails to
	// find the JSONL in the matching project dir, and exits immediately.
	if opts.Project != "" {
		setSessionProject(id, opts.Project)
	}
	if opts.SpawnedBy != "" {
		updateSpawnedBy(id, opts.SpawnedBy)
	}
	if opts.GalID != "" {
		setGalID(id, opts.GalID)
	}
	if opts.ReplaceSoul {
		setReplaceSoulEnabled(id, true)
	}
	// Persist soul_enabled so rehydration can rebuild bare sessions correctly.
	// Default column value is 1; only write when false to keep UPDATE traffic low.
	if !opts.Soul {
		setSoulEnabledDB(id, false)
	}
	// Phase D — persist the initial soul-patch state (loaded fragments + mode)
	// so a server restart that bounces this session can restore the same
	// "already loaded" set instead of re-patching everything on the first
	// post-restart user turn.
	if len(sess.LoadedFragments) > 0 || sess.CurrentSoulMode != "" {
		setSoulPatchState(id, sess.LoadedFragments, sess.PendingPatches, sess.CurrentSoulMode)
	}

	// Attach stdout/event → SSE bridge, exit watcher, and stderr drain.
	// Dispatch on Backend type — codex flows through attachCodexBridge,
	// CC keeps the existing stream-json bridge.
	switch p := proc.(type) {
	case *codexBackend:
		attachCodexBridge(p, sess, "server-create", true)
	case *grokBackend:
		attachGrokBridge(p, sess, "server-create", true)
	case *kimiBackend:
		attachKimiBridge(p, sess, "server-create", true)
	case *agyBackend:
		attachAgyBridge(p, sess, "server-create", true)
	case *opencodeBackend:
		attachOpencodeBridge(p, sess, "server-create", true)
	case *claudeBackend:
		attachProcessBridge(p, sess, "server-create", true)
	default:
		// Should never happen — every Backend in tree is one of the above.
		fmt.Fprintf(os.Stderr, "[%s] server: unknown backend type %T for session %s\n",
			appName, proc, shortID(id))
	}

	return sess, nil
}

// getSession returns a session by ID.
func (sm *sessionManager) getSession(id string) *serverSession {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[id]
}

// listSessions returns snapshots of all active sessions.
func (sm *sessionManager) listSessions() []map[string]any {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	list := make([]map[string]any, 0, len(sm.sessions))
	for _, sess := range sm.sessions {
		list = append(list, sess.snapshot())
	}
	return list
}

// setChrome reloads a session's underlying claude process with --chrome
// toggled on/off. The serverSession (broadcaster, history, name, etc.) is
// preserved across the reload — only the subprocess is swapped.
//
// Alternate backends: chrome is a CC-only concept. Returning an error here
// prevents silently swapping the backend subprocess for a fresh CC process
// and losing thread state. The HTTP handler surfaces the error to the
// caller as 5xx.
func (sm *sessionManager) setChrome(id string, enabled bool) error {
	sess := sm.getSession(id)
	if sess == nil {
		return fmt.Errorf("session not found: %s", id)
	}

	sess.mu.Lock()
	if sess.Backend != BackendCC {
		backend := sess.Backend
		sess.mu.Unlock()
		return fmt.Errorf("setChrome not supported on %s backend", backend)
	}
	if sess.ChromeEnabled == enabled && sess.process != nil && sess.process.alive() {
		sess.mu.Unlock()
		return nil // no-op
	}
	oldProc := sess.process
	sess.process = nil
	// Preserve conversation history across the reload. Without ResumeID the
	// respawned claude process would start a fresh cc session, orphaning the
	// existing jsonl and breaking the in-progress conversation.
	resumeID := sess.ClaudeSID
	opts := sessionOpts{
		WorkDir:          sess.Project,
		SystemPromptFile: sess.promptFile,
		Model:            sess.Model,
		MCPConfig:        sess.mcpConfig,
		Chrome:           enabled,
		ResumeID:         resumeID,
		ReplaceSoul:      sess.ReplaceSoul,
		ServerSessionID:  sess.ID,
	}
	sess.mu.Unlock()

	// Mark the old process so its bridgeStdout doesn't broadcast a "close"
	// SSE event when it exits — this is an intentional reload.
	if oldProc != nil {
		oldProc.suppressNextClose()
		if oldProc.alive() {
			oldProc.shutdown()
		}
	}

	// Wait for old bridge goroutine to exit before starting a new one,
	// preventing two bridges writing to the same broadcaster concurrently.
	sess.waitBridgeDone()

	sess.setStatus("starting")

	// Spawn replacement
	newProc, err := spawnClaude(opts)
	if err != nil {
		sess.setStatus("error")
		return fmt.Errorf("respawn claude: %w", err)
	}

	sess.mu.Lock()
	sess.process = newProc
	sess.ChromeEnabled = enabled
	// Reset Claude session ID — the new subprocess will report its own.
	sess.ClaudeSID = ""
	sess.mu.Unlock()

	setChromeEnabled(id, enabled)
	sess.setStatus("running")

	// Re-attach stdout bridge (reload — no agent recording, no rename sync)
	attachProcessBridge(newProc, sess, "", false)
	applyPersistedEffort(sess)

	if sm.hub != nil {
		sm.hub.notifySessions()
	}
	fmt.Fprintf(os.Stderr, "[%s] server: session %s chrome=%v reloaded\n", appName, shortID(id), enabled)
	return nil
}

// setReplaceSoul is a thin compatibility wrapper around setMode. Kept so the
// legacy POST /api/sessions/{id}/replace-soul endpoint still works.
func (sm *sessionManager) setReplaceSoul(id string, enabled bool) error {
	if enabled {
		return sm.setMode(id, ModeBenwo)
	}
	// Disabling replace on a bare session should stay bare; otherwise go back to weiran.
	if sess := sm.getSession(id); sess != nil {
		sess.mu.Lock()
		soul := sess.SoulEnabled
		sess.mu.Unlock()
		if !soul {
			return sm.setMode(id, ModeCC)
		}
	}
	return sm.setMode(id, ModeWeiran)
}

// setMode reloads a session's underlying claude process under one of the three
// session modes (weiran/benwo/cc). The serverSession (broadcaster, history,
// name, etc.) is preserved across the reload — only the subprocess is swapped.
// Switching modes mid-session shifts the model's identity for the rest of the
// conversation; the UI surfaces a warning.
func (sm *sessionManager) setMode(id, mode string) error {
	soulEnabled, replaceSoul, ok := modeToFlags(mode)
	if !ok {
		return fmt.Errorf("unknown mode: %q (expected weiran/benwo/cc)", mode)
	}
	sess := sm.getSession(id)
	if sess == nil {
		return fmt.Errorf("session not found: %s", id)
	}

	sess.mu.Lock()
	// Alternate backends: mode (weiran/benwo/cc) is a CC system-prompt concept.
	// Reloading would silently swap the backend subprocess for a CC process
	// and lose thread state. Return an error instead.
	if sess.Backend != BackendCC {
		backend := sess.Backend
		sess.mu.Unlock()
		return fmt.Errorf("setMode not supported on %s backend", backend)
	}
	// No-op if already in target mode and process alive
	if sess.SoulEnabled == soulEnabled && sess.ReplaceSoul == replaceSoul &&
		sess.process != nil && sess.process.alive() {
		sess.mu.Unlock()
		return nil
	}
	oldProc := sess.process
	sess.process = nil
	resumeID := sess.ClaudeSID // preserve conversation history across reload
	existingPromptFile := sess.promptFile
	workDir := sess.Project
	model := sess.Model
	mcpConfig := sess.mcpConfig
	chrome := sess.ChromeEnabled
	sess.mu.Unlock()

	// Resolve prompt file for the new mode.
	//   - cc mode → empty (no --*-system-prompt-file flag)
	//   - weiran/benwo mode → rebuild if missing, else reuse existing
	var promptFile string
	if soulEnabled {
		if existingPromptFile != "" {
			promptFile = existingPromptFile
		} else {
			initSessionDir()
			// Mode switch / rehydrate on existing CC session — skip CC summaries
			// since --resume <ccID> will restore full conversation history.
			promptFile = writePromptForSession(id, buildPromptWithOverrides(promptOverrides{SkipCCSessions: true}))
		}
	}

	opts := sessionOpts{
		WorkDir:          workDir,
		SystemPromptFile: promptFile,
		Model:            model,
		MCPConfig:        mcpConfig,
		Chrome:           chrome,
		ReplaceSoul:      replaceSoul,
		ResumeID:         resumeID,
		ServerSessionID:  sess.ID,
	}

	if oldProc != nil {
		oldProc.suppressNextClose()
		if oldProc.alive() {
			oldProc.shutdown()
		}
	}

	// Wait for old bridge goroutine to exit before starting a new one
	sess.waitBridgeDone()

	sess.setStatus("starting")

	newProc, err := spawnClaude(opts)
	if err != nil {
		sess.setStatus("error")
		return fmt.Errorf("respawn claude: %w", err)
	}

	sess.mu.Lock()
	sess.process = newProc
	sess.SoulEnabled = soulEnabled
	sess.ReplaceSoul = replaceSoul
	sess.promptFile = promptFile
	sess.ClaudeSID = ""
	sess.mu.Unlock()

	setReplaceSoulEnabled(id, replaceSoul)
	setSoulEnabledDB(id, soulEnabled)
	sess.setStatus("running")

	attachProcessBridge(newProc, sess, "", false)
	applyPersistedEffort(sess)

	if sm.hub != nil {
		sm.hub.notifySessions()
	}
	fmt.Fprintf(os.Stderr, "[%s] server: session %s mode=%s (soul=%v replace=%v) reloaded\n",
		appName, shortID(id), mode, soulEnabled, replaceSoul)
	return nil
}

// setModel reloads the underlying claude process with a different model
// (e.g. "zai/glm-5.1"). All session state — conversation history,
// replace_soul, chrome, mcp config — is preserved across the reload.
func (sm *sessionManager) setModel(id string, model string) error {
	sess := sm.getSession(id)
	if sess == nil {
		return fmt.Errorf("session not found: %s", id)
	}

	sess.mu.Lock()
	// Alternate backends do not share CC's reload/resume semantics. Return
	// an error rather than silently swapping for a CC process.
	if sess.Backend != BackendCC {
		backend := sess.Backend
		sess.mu.Unlock()
		return fmt.Errorf("setModel not supported on %s backend", backend)
	}
	if sess.Model == model && sess.process != nil && sess.process.alive() {
		sess.mu.Unlock()
		return nil // no-op
	}
	oldProc := sess.process
	sess.process = nil
	resumeID := sess.ClaudeSID // preserve conversation history
	opts := sessionOpts{
		WorkDir:          sess.Project,
		SystemPromptFile: sess.promptFile,
		Model:            model,
		MCPConfig:        sess.mcpConfig,
		Chrome:           sess.ChromeEnabled,
		ReplaceSoul:      sess.ReplaceSoul,
		ResumeID:         resumeID,
		ServerSessionID:  sess.ID,
	}
	sess.mu.Unlock()

	if oldProc != nil {
		oldProc.suppressNextClose()
		if oldProc.alive() {
			oldProc.shutdown()
		}
	}

	// Wait for old bridge goroutine to exit before starting a new one
	sess.waitBridgeDone()

	sess.setStatus("starting")

	newProc, err := spawnClaude(opts)
	if err != nil {
		sess.setStatus("error")
		return fmt.Errorf("respawn claude: %w", err)
	}

	sess.mu.Lock()
	sess.process = newProc
	sess.Model = model
	sess.ClaudeSID = ""
	sess.mu.Unlock()

	// Persist model for rehydration
	setSessionModel(sess.ID, model)

	sess.setStatus("running")

	attachProcessBridge(newProc, sess, "", false)
	applyPersistedEffort(sess)

	if sm.hub != nil {
		sm.hub.notifySessions()
	}
	fmt.Fprintf(os.Stderr, "[%s] server: session %s model=%s reloaded\n", appName, shortID(id), model)
	return nil
}

// destroySession gracefully shuts down and removes a session.
func (sm *sessionManager) destroySession(id string) error {
	sm.mu.Lock()
	sess, ok := sm.sessions[id]
	if !ok {
		sm.mu.Unlock()
		return fmt.Errorf("session not found: %s", id)
	}
	delete(sm.sessions, id)
	sm.mu.Unlock()

	sess.setStatus("stopped")
	sess.mu.Lock()
	proc := sess.process
	sess.process = nil
	sess.mu.Unlock()

	if proc != nil && proc.alive() {
		proc.shutdown()
	}

	// Mark as ended in persistent DB
	updateSessionStatus(id, "ended")

	// Clean up temp prompt file
	if sess.promptFile != "" {
		os.Remove(sess.promptFile)
	}

	// Clean up persistent per-session state (chrome flag, rename meta, etc.)
	clearSessionRow(id)

	// Clear telegram chat→session mapping if this was a TG session
	if sess.Category == CategoryTelegram {
		clearTGSessionMapping(id)
	}

	if sm.hub != nil {
		sm.hub.notifySessions()
	}
	return nil
}

// shutdownAll gracefully stops all sessions. Used on server shutdown.
// Marks rehydratable sessions as "suspended" in DB before destroying processes,
// so they can be resumed on next server startup.
func (sm *sessionManager) shutdownAll() {
	close(sm.stopReaper)
	// Wait for reaper to fully exit before suspending sessions.
	// Prevents race where reaper's destroySession overwrites "suspended" → "ended".
	sm.reaperDone.Wait()

	// Persist claude_session_id and model for all active sessions before they die,
	// so they can be properly rehydrated after restart.
	sm.mu.RLock()
	for _, sess := range sm.sessions {
		sess.mu.Lock()
		sid := sess.ClaudeSID
		id := sess.ID
		model := sess.Model
		sess.mu.Unlock()
		if sid != "" {
			setClaudeSessionID(id, sid)
		}
		fmt.Fprintf(os.Stderr, "[%s] server: shutdownAll: session %s model=%q claude_sid=%s\n",
			appName, shortID(id), model, shortID(sid))
		if model != "" {
			setSessionModel(id, model)
		}
	}
	sm.mu.RUnlock()

	n := batchSuspendActiveSessions()
	if n > 0 {
		fmt.Fprintf(os.Stderr, "[%s] server: suspended %d session(s) for rehydration\n", appName, n)
	}

	sm.mu.Lock()
	ids := make([]string, 0, len(sm.sessions))
	for id := range sm.sessions {
		ids = append(ids, id)
	}
	sm.mu.Unlock()

	for _, id := range ids {
		// destroySession calls updateSessionStatus("ended"), but we want suspended
		// sessions to stay suspended. So we skip the status update for suspended sessions.
		sm.destroySessionForShutdown(id)
	}
}

// destroySessionForShutdown is like destroySession but does NOT update DB status,
// preserving the "suspended" state for rehydration.
func (sm *sessionManager) destroySessionForShutdown(id string) {
	sm.mu.Lock()
	sess, ok := sm.sessions[id]
	if !ok {
		sm.mu.Unlock()
		return
	}
	delete(sm.sessions, id)
	sm.mu.Unlock()

	sess.setStatus("stopped")
	sess.mu.Lock()
	proc := sess.process
	sess.process = nil
	sess.mu.Unlock()

	if proc != nil && proc.alive() {
		proc.shutdown()
	}
	if sess.promptFile != "" {
		os.Remove(sess.promptFile)
	}
	// Do NOT call updateSessionStatus — keep "suspended" in DB
}

// reaper runs every minute and destroys idle/expired sessions.
func (sm *sessionManager) reaper() {
	defer sm.reaperDone.Done()
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-sm.stopReaper:
			return
		case <-ticker.C:
			sm.reap()
		}
	}
}

func (sm *sessionManager) reap() {
	now := time.Now()

	sm.mu.RLock()
	var toDestroy []string
	for id, sess := range sm.sessions {
		sess.mu.Lock()
		idle := now.Sub(sess.LastActive) > sm.idleTimeout
		expired := now.Sub(sess.CreatedAt) > sm.maxLifetime
		dead := sess.process != nil && !sess.process.alive()
		category := sess.Category
		sess.mu.Unlock()

		// Ephemeral sessions (heartbeat/cron/evolve): destroy as soon as process exits
		if isEphemeralCategory(category) && dead {
			toDestroy = append(toDestroy, id)
			continue
		}

		// Telegram sessions: destroy on idle/expired but not on dead (will be recreated on next message)
		if category == CategoryTelegram {
			if idle || expired {
				toDestroy = append(toDestroy, id)
			}
			continue
		}

		// Interactive sessions: normal TTL rules
		if idle || expired || dead {
			toDestroy = append(toDestroy, id)
		}
	}
	sm.mu.RUnlock()

	for _, id := range toDestroy {
		fmt.Fprintf(os.Stderr, "[%s] server: reaping session %s\n", appName, shortID(id))
		sm.destroySession(id)
	}
}

// ── Resume Session ──

// resumeSession reactivates a session wrapping `claude --resume <ccID>`.
//
// Identity model (post weiran-id-stable-on-resume):
//   - The input `inputID` may be either a weiran session id or a Claude Code
//     session id; resolveResumeIDs normalizes it to (weiranID, ccID).
//   - The returned session reuses `weiranID` as its ID — stable across every
//     resume / rehydrate cycle. Web UI bookmarks, IPC keys, and message /
//     proxy_requests foreign keys all keep pointing at the same id.
//   - `ccID` is only handed to `claude --resume` so the CC subprocess appends
//     to the existing jsonl. CC itself preserves the cc id across --resume
//     unless --fork-session is specified (verified against CC source).
//
// If a session with the same weiran id is already active, this is a no-op
// and the existing session is returned.
//
// replaceSoul / soulEnabled pointers follow their historical contract:
// nil means "inherit from persisted DB flag", non-nil means "use this value".
func (sm *sessionManager) resumeSession(inputID, message, displayName, categoryOverride, model string, replaceSoul *bool, soulEnabled *bool) (*serverSession, error) {
	weiranID, ccID, _ := resolveResumeIDs(inputID)
	id := weiranID
	now := time.Now()

	if displayName == "" {
		// Short id from cc side — that's what users recognize from `claude -r` land.
		displayName = "resume-" + shortID(ccID)
	}

	bc := newBroadcaster()
	bc.hub = sm.hub
	bc.sessionID = id

	// Category priority: explicit override > DB record > default.
	// getSessionCategory accepts either weiran or cc id (WHERE session_id=? OR claude_session_id=?).
	resolvedCategory := categoryOverride
	if resolvedCategory == "" {
		resolvedCategory = getSessionCategory(id)
	}
	if resolvedCategory == "" {
		resolvedCategory = CategoryInteractive
	}

	// Model priority: explicit arg > DB record > JSONL init message.
	// JSONL lookup MUST use cc id — the file on disk is named after cc id.
	origModel := model
	if model == "" {
		model = getSessionModel(id)
	}
	if model == "" {
		model = getModelFromJSONL(ccID)
	}
	fmt.Fprintf(os.Stderr, "[%s] server: resumeSession model trace: arg=%q → resolved=%q (weiran=%s cc=%s)\n",
		appName, origModel, model, shortID(id), shortID(ccID))

	// Resolve replace_soul: explicit arg wins, else inherit from persisted DB flag
	resolvedReplaceSoul := false
	if replaceSoul != nil {
		resolvedReplaceSoul = *replaceSoul
	} else {
		resolvedReplaceSoul = getReplaceSoulEnabled(id)
	}

	// Resolve soul_enabled: explicit arg wins, else inherit (defaults true).
	// A bare/CC-mode session resumes without attaching the soul prompt.
	resolvedSoulEnabled := true
	if soulEnabled != nil {
		resolvedSoulEnabled = *soulEnabled
	} else {
		resolvedSoulEnabled = getSoulEnabledDB(id)
	}
	if !resolvedSoulEnabled {
		// bare mode has no soul → replace_soul is meaningless, force to false
		resolvedReplaceSoul = false
	}

	// Inherit first_msg from original session (jsonl lookup uses cc id).
	inheritedFirstMsg := getFirstMsgFromJSONL(ccID)

	// Resolve the project (cwd) to relaunch CC in. Priority:
	//   1. server_sessions.project column (persisted on create)
	//   2. cwd recorded in the JSONL (backfill for pre-migration sessions)
	//   3. workspace default (brand-new sessions without transcripts)
	// Without this, sessions spawned outside the default workspace die on
	// resume: CC launches in workspace, can't find the transcript in the
	// matching ~/.claude/projects/<encoded-cwd>/<cc>.jsonl path, and exits.
	resolvedProject := getSessionProject(id)
	projectSource := "db"
	if resolvedProject == "" {
		if jp := getProjectFromJSONL(ccID); jp != "" {
			resolvedProject = jp
			projectSource = "jsonl-backfill"
			// Backfill the DB so subsequent resumes hit the fast path.
			setSessionProject(id, jp)
		}
	}
	if resolvedProject == "" {
		resolvedProject = workspace
		projectSource = "workspace-default"
	}
	fmt.Fprintf(os.Stderr, "[%s] server: resumeSession project=%q (source=%s weiran=%s cc=%s)\n",
		appName, resolvedProject, projectSource, shortID(id), shortID(ccID))

	loadedFragments, pendingPatches, currentSoulMode := getSoulPatchState(id)

	sess := &serverSession{
		ID:       id,
		Name:     displayName,
		FirstMsg: inheritedFirstMsg,
		// ResumedFrom is no longer set on internal resume — the weiran id is
		// stable, so there is no "from". Kept as a field for backward compat
		// with persisted data and for future --fork-session support.
		ResumedFrom:     "",
		ClaudeSID:       ccID, // pre-populate; init message will confirm (same value expected)
		Project:         resolvedProject,
		Model:           model,
		Status:          "starting",
		Category:        resolvedCategory,
		CreatedAt:       now,
		LastActive:      now,
		SoulEnabled:     resolvedSoulEnabled,
		ReplaceSoul:     resolvedReplaceSoul,
		LoadedFragments: loadedFragments,
		PendingPatches:  pendingPatches,
		CurrentSoulMode: currentSoulMode,
		StreamURL:       fmt.Sprintf("/api/sessions/%s/stream", id),
		broadcaster:     bc,
		hub:             sm.hub,
		tasks:           newTaskTracker(),
	}

	// Atomic slot reservation:
	//   1. If an active session already holds this weiran id, return it (no-op).
	//   2. If a dead session holds the slot, clear it so we can replace in-place.
	//   3. If a different weiran session is currently wrapping the same cc id
	//      (legacy data only — post-migration this is impossible), defer to it.
	sm.mu.Lock()
	if existing, ok := sm.sessions[id]; ok {
		existing.mu.Lock()
		alive := existing.process != nil && existing.process.alive()
		existing.mu.Unlock()
		if alive {
			sm.mu.Unlock()
			return existing, nil
		}
		delete(sm.sessions, id)
	}
	for otherID, s := range sm.sessions {
		if otherID == id {
			continue
		}
		s.mu.Lock()
		cid := s.ClaudeSID
		alive := s.process != nil && s.process.alive()
		s.mu.Unlock()
		if alive && cid == ccID {
			sm.mu.Unlock()
			return s, nil
		}
	}
	sm.sessions[id] = sess
	sm.mu.Unlock()

	cleanup := func() {
		sm.mu.Lock()
		delete(sm.sessions, id)
		sm.mu.Unlock()
	}

	// Build soul prompt — per-session file to avoid concurrent overwrites.
	// Skip entirely when resuming a bare/CC-mode session (SystemPromptFile="").
	var promptFile string
	if resolvedSoulEnabled {
		// Resume path — CC --resume <ccID> restores full history, so skip
		// the "Recent Claude Code session summaries" injection. Stacking the
		// summary on every resume is what caused prompt bloat. relay/review
		// skills spawn fresh sessions via createSession and still get the
		// summary there (they have no CC history to resume from).
		promptFile = writePromptForSession(id, buildPromptWithOverrides(promptOverrides{SkipCCSessions: true}))
		sess.promptFile = promptFile
	}

	// Decide whether an auto-wake ping is needed BEFORE spawning CC.
	// Running transcriptNeedsWake after spawnClaude races with CC's own
	// writes to the transcript: CC --resume immediately begins appending
	// init/system records, and in rare cases a real response, so reading
	// the transcript mid-spawn can observe a half-written state or CC's
	// own new records rather than the pre-resume snapshot. Moving the
	// decision here guarantees we judge the transcript as it was at the
	// moment resume was requested.
	// Auto-wake disabled: injecting "." on resume still has bugs
	// (e.g. CC sometimes treats it as a real user turn, or doesn't
	// actually unstick the conversation). Leaving message empty —
	// callers must explicitly send a kickoff message if needed.
	autoWoken := false
	_ = autoWoken
	_ = transcriptNeedsWake // keep helper alive for future re-enable

	// Spawn Claude Code with --resume <ccID>
	// WorkDir MUST be the original project path — CC looks up the transcript
	// via cwd-encoded project dir (~/.claude/projects/<encoded-cwd>/<ccID>.jsonl).
	// Without this, spawn sessions created outside the default workspace fail
	// with "No conversation found with session ID: <ccID>".
	proc, err := spawnClaude(sessionOpts{
		SystemPromptFile: promptFile,
		Model:            model,
		ResumeID:         ccID,
		ReplaceSoul:      resolvedReplaceSoul,
		ServerSessionID:  id,
		WorkDir:          resolvedProject,
	})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("spawn resume: %w", err)
	}

	sess.mu.Lock()
	sess.process = proc
	// Inherit persisted effort so the freshly-spawned CC subprocess starts on
	// the same reasoning tier the user picked before the resume / restart.
	sess.Effort = getEffortDB(id)
	sess.mu.Unlock()
	sess.setStatus("running")

	// Register / refresh in server DB.
	// ensureServerSession is INSERT ... ON CONFLICT DO UPDATE, so this both
	// revives a prior row for this weiran id and bumps updated_at.
	ensureServerSession(id, sess.Name)
	setClaudeSessionID(id, ccID)
	updateSessionStatus(id, "active")
	if model != "" {
		setSessionModel(id, model)
	}
	// Persist the resolved project so subsequent resumes (and rehydrate after
	// server restart) reuse the same cwd without repeating JSONL lookup.
	if resolvedProject != "" && resolvedProject != workspace {
		setSessionProject(id, resolvedProject)
	}
	if resolvedReplaceSoul {
		setReplaceSoulEnabled(id, true)
	}
	if !resolvedSoulEnabled {
		setSoulEnabledDB(id, false)
	}

	// Attach stdout bridge with full sync (agent recording + CC name sync + auto-rename)
	attachProcessBridge(proc, sess, "server-resume", true)
	applyPersistedEffort(sess)

	// Send initial message if provided — wait for Claude Code to emit init
	// before writing to stdin (consistent with createSession's waitInit pattern).
	// The auto-wake decision was made above (before spawnClaude) and may have
	// promoted an empty message to "." with autoWoken=true.
	if message != "" {
		// Only promote a real caller message to FirstMsg. Auto-wake pings
		// must not overwrite FirstMsg — that field is used as a session
		// label / summary and "." would be meaningless.
		if !autoWoken {
			sess.mu.Lock()
			if sess.FirstMsg == "" {
				sess.FirstMsg = message
			}
			sess.mu.Unlock()
		}
		// A backend that died during its handshake cannot accept this
		// message: delivering it is a no-op that leaves the resume looking
		// successful while nothing was ever sent. awaitBackendReady marks the
		// session terminal/"error" in that case, and we skip the injection.
		if sess.awaitBackendReady(30 * time.Second) {
			// Use resume-only variant: skip topic fragment detection so
			// infrastructure messages (restart notices) don't trigger injection.
			injection := sess.prepareSoulPatchResumeOnly(message)
			userEvent, _ := json.Marshal(map[string]any{
				"type":    "user",
				"message": map[string]any{"role": "user", "content": injection.DisplayMessage},
			})
			sess.broadcaster.broadcast(sseEvent{Event: "user", Data: userEvent})
			// Log sendMessage failures instead of silently dropping them. A
			// silent drop on an auto-wake makes it look like resume succeeded
			// while CC actually never received the ping — masking whether the
			// underlying "resume self-exits" bug is still present.
			if err := proc.sendMessage(injection.Outbound); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] server: resume sendMessage failed weiran=%s auto_wake=%v err=%v\n",
					appName, shortID(sess.ID), autoWoken, err)
			} else {
				sess.commitSoulPatchInjection(injection)
			}
		} else {
			fmt.Fprintf(os.Stderr, "[%s] server: resume %s skipped message — backend failed to initialize\n",
				appName, shortID(sess.ID))
		}
	}
	if autoWoken {
		fmt.Fprintf(os.Stderr, "[%s] server: resumed weiran=%s cc=%s (auto-woken: transcript closed)\n",
			appName, shortID(id), shortID(ccID))
	} else {
		fmt.Fprintf(os.Stderr, "[%s] server: resumed weiran=%s cc=%s\n",
			appName, shortID(id), shortID(ccID))
	}
	return sess, nil
}

// readClaudeSessionName reads the session name from Claude Code's session meta file.
func readClaudeSessionName(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	// Claude Code stores session meta as numeric-ID .json files,
	// each containing a "sessionId" (UUID) and optional "name".
	sessDir := filepath.Join(claudeConfigDir, "sessions")
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
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
		if json.Unmarshal(data, &meta) == nil && meta.SessionID == sessionID && meta.Name != "" {
			return meta.Name
		}
	}
	return ""
}

// ── Session Rehydration ──

const rehydrateMaxAge = 2 * time.Hour

// rehydrateSessions restores interactive/telegram sessions that were alive
// before the last server restart. Called once during server startup.
func (sm *sessionManager) rehydrateSessions() {
	sessions, err := getRehydratableSessions(rehydrateMaxAge)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] server: rehydrate: query error: %v\n", appName, err)
		return
	}
	if len(sessions) == 0 {
		fmt.Fprintf(os.Stderr, "[%s] server: rehydrate: no sessions to restore\n", appName)
		// Clean up any stale active/suspended records
		expireStaleRehydratables(rehydrateMaxAge)
		return
	}

	fmt.Fprintf(os.Stderr, "[%s] server: rehydrate: found %d session(s) to restore\n", appName, len(sessions))

	restored := 0
	for _, s := range sessions {
		// Verify the JSONL file exists (claude session is resumable)
		jsonlPath := findSessionJSONL(s.ClaudeSID)
		if jsonlPath == "" {
			fmt.Fprintf(os.Stderr, "[%s] server: rehydrate: skip %s (no JSONL for %s)\n",
				appName, shortID(s.SessionID), shortID(s.ClaudeSID))
			updateSessionStatus(s.SessionID, "ended")
			continue
		}

		// Determine the resume message:
		// - Restart initiator: use their custom rehydrate_message
		// - Bystander sessions: inject a server-restart context notice so the
		//   model knows it was interrupted and won't hallucinate completed tool calls
		resumeMsg := s.RehydrateMsg
		if resumeMsg == "" {
			// Bystander path: this session did NOT trigger the restart. Some
			// other session ran `make server-restart` (in which case the
			// mark_restart_initiator tool-hook tagged THAT session and it
			// gets a custom message instead) or an external actor restarted
			// launchd. Either way, in-flight tool calls in THIS session did
			// not complete.
			resumeMsg = "⚠️ Server was restarted by another session or an external action — " +
				"NOT by you. Any in-flight tool calls (Bash, etc.) you had running were killed " +
				"and did NOT complete. Do NOT assume they succeeded. Report your current status " +
				"and wait for instructions. Do NOT trigger another restart."
		}

		fmt.Fprintf(os.Stderr, "[%s] server: rehydrate: restoring weiran=%s cc=%s model=%q\n",
			appName, shortID(s.SessionID), shortID(s.ClaudeSID), s.Model)
		replaceSoul := s.ReplaceSoul
		soulEnabled := s.SoulEnabled
		// Pass the weiran id (not the cc id) so resumeSession reuses it —
		// the weiran id stays stable across server restarts. resumeSession
		// internally looks up the cc id and feeds it to `claude --resume`.
		sess, err := sm.resumeSession(
			s.SessionID, // weiran id — stable across resume
			resumeMsg,   // always send a message (context notice or wake command)
			s.Name,      // display name
			s.Category,  // category override
			s.Model,     // model
			&replaceSoul,
			&soulEnabled,
		)
		// Clear one-shot rehydrate message regardless of outcome.
		if s.RehydrateMsg != "" {
			setRehydrateMessage(s.SessionID, "")
		}

		if err != nil {
			// Mark the row ended only on hard failure — success path keeps
			// the same weiran id and updates status to "active" internally.
			updateSessionStatus(s.SessionID, "ended")
			fmt.Fprintf(os.Stderr, "[%s] server: rehydrate: failed %s: %v\n",
				appName, shortID(s.SessionID), err)
			continue
		}

		// Carry over chrome/gal state. With weiran-id-stable-on-resume,
		// sess.ID == s.SessionID, so these writes refresh the same row.
		if s.ChromeEnabled {
			setChromeEnabled(sess.ID, true)
		}
		if s.GalID != "" {
			setGalID(sess.ID, s.GalID)
		}
		// Per-session effort restoration is handled inside resumeSession
		// (it loads from getEffortDB and calls applyPersistedEffort after
		// process spawn), so no extra work here. The Effort field on
		// rehydratableSession is captured purely for diagnostics / future use.
		_ = s.Effort

		// Phase D — soul-patch state is already restored inside resumeSession
		// (via getSoulPatchState reading the authoritative DB value). Do NOT
		// overwrite here with the stale snapshot from getRehydratableSessions
		// which was captured before resumeSession's commitSoulPatchInjection
		// could update the DB. The old overwrite caused LoadedFragments to
		// regress to the pre-resume snapshot, making fragments appear "new"
		// again on the next user message.

		wakeNote := "idle"
		if s.RehydrateMsg != "" {
			wakeNote = "wake"
		}
		fmt.Fprintf(os.Stderr, "[%s] server: rehydrate: restored weiran=%s cc=%s (%s, %s)\n",
			appName, shortID(sess.ID), shortID(sess.ClaudeSID), s.Category, wakeNote)
		restored++
	}

	// Expire any remaining stale sessions
	expireStaleRehydratables(rehydrateMaxAge)

	fmt.Fprintf(os.Stderr, "[%s] server: rehydrate: restored %d/%d session(s)\n",
		appName, restored, len(sessions))

	// Notify WS clients about restored sessions
	if sm.hub != nil {
		sm.hub.notifySessions()
	}
}

// ── JSONL History Parsing ──

// findSessionJSONL locates the JSONL file for a given session ID.
// Searches both Claude Code projects and OpenClaw agent session directories.
func findSessionJSONL(sessionID string) string {
	fname := sessionID + ".jsonl"

	// Claude Code sessions: ~/.claude/projects/*/
	claudeProjects := filepath.Join(claudeConfigDir, "projects")
	if entries, err := os.ReadDir(claudeProjects); err == nil {
		for _, projEntry := range entries {
			if !projEntry.IsDir() {
				continue
			}
			path := filepath.Join(claudeProjects, projEntry.Name(), fname)
			if _, err := os.Stat(path); err == nil {
				return path
			}
		}
	}

	// Codex transcripts: <appDir>/codex/
	codexPath := filepath.Join(appDir, "codex", fname)
	if _, err := os.Stat(codexPath); err == nil {
		return codexPath
	}

	return ""
}

// historyMessage is a parsed message from a session JSONL for the UI.
type historyMessage struct {
	Role      string         `json:"role"`                  // user, assistant, system, tool_use, tool_result, image
	Content   string         `json:"content"`               // text content
	ToolName  string         `json:"tool_name,omitempty"`   // for tool_use
	ToolInput string         `json:"tool_input,omitempty"`  // for tool_use
	ToolUseID string         `json:"tool_use_id,omitempty"` // for tool_use / tool_result (pair them in UI)
	Timestamp string         `json:"timestamp,omitempty"`
	Images    []historyImage `json:"images,omitempty"` // base64 images from tool results
}

type historyImage struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// parseSessionMessages reads a JSONL file and extracts the last N messages for display.
func parseSessionMessages(path string, limit int) []historyMessage {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var all []historyMessage

	scanner := bufio.NewScanner(f)
	// Claude Code JSONL lines can contain large base64 images or fetched HTML
	// (seen >1 MB in practice). Use a generous 64 MB max to avoid
	// "bufio.Scanner: token too long" truncating the tail of the history.
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}

		var ev struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
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
		case "user":
			// Check for images in tool_result content blocks
			images := extractImages(ev.Message.Content)
			if len(images) > 0 {
				all = append(all, historyMessage{
					Role:      "image",
					Images:    images,
					Timestamp: ev.Timestamp,
				})
			}
			text := extractContentText(ev.Message.Content)
			// Phase D — strip any `<<soul-patch>>...<</soul-patch>>` blocks
			// before surfacing user history to the UI. Patches are server
			// side persona augmentation, not human-typed content; the
			// frontend should never render them in the chat pane.
			text = stripSoulPatchBlocks(text)
			if text != "" {
				all = append(all, historyMessage{
					Role:      "user",
					Content:   text,
					Timestamp: ev.Timestamp,
				})
			}

		case "system":
			var sysPeek struct {
				Subtype     string `json:"subtype"`
				CompactMeta struct {
					Trigger    string `json:"trigger"`
					PreTokens  int    `json:"preTokens"`
					PreTokens2 int    `json:"pre_tokens"`
				} `json:"compactMetadata"`
				CompactMeta2 struct {
					Trigger    string `json:"trigger"`
					PreTokens  int    `json:"preTokens"`
					PreTokens2 int    `json:"pre_tokens"`
				} `json:"compact_metadata"`
			}
			if json.Unmarshal(line, &sysPeek) == nil && sysPeek.Subtype == "compact_boundary" {
				// Merge both naming conventions: pick non-zero from either struct
				trigger := sysPeek.CompactMeta.Trigger
				if trigger == "" {
					trigger = sysPeek.CompactMeta2.Trigger
				}
				tokens := sysPeek.CompactMeta.PreTokens
				if tokens == 0 {
					tokens = sysPeek.CompactMeta.PreTokens2
				}
				if tokens == 0 {
					tokens = sysPeek.CompactMeta2.PreTokens
				}
				if tokens == 0 {
					tokens = sysPeek.CompactMeta2.PreTokens2
				}
				all = append(all, historyMessage{
					Role:      "compact_boundary",
					Content:   fmt.Sprintf("compact_boundary|%s|%d", trigger, tokens),
					Timestamp: ev.Timestamp,
				})
			}

		case "assistant":
			var blocks []struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				Name      string          `json:"name"`
				ID        string          `json:"id"`          // tool_use id
				ToolUseID string          `json:"tool_use_id"` // tool_result → tool_use linkage
				Input     json.RawMessage `json:"input"`
			}
			if json.Unmarshal(ev.Message.Content, &blocks) != nil {
				continue
			}

			var textParts []string
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if b.Text != "" {
						textParts = append(textParts, b.Text)
					}
				case "tool_use":
					// Flush any accumulated text first
					if len(textParts) > 0 {
						all = append(all, historyMessage{
							Role:      "assistant",
							Content:   strings.Join(textParts, "\n"),
							Timestamp: ev.Timestamp,
						})
						textParts = nil
					}
					inputStr := string(b.Input)
					// Keep full input for tools the frontend renders structurally
					// (diffs, command preview, file path, etc.). Truncating these
					// breaks JSON parsing on the client and falls back to a raw
					// JSON dump — which is exactly the ugly state we're avoiding.
					switch b.Name {
					case "Edit", "Write", "NotebookEdit", "MultiEdit",
						"Bash", "Read", "Glob", "Grep", "WebFetch", "WebSearch", "TodoWrite":
						// no truncation
					default:
						if len(inputStr) > 500 {
							inputStr = inputStr[:500] + "..."
						}
					}
					all = append(all, historyMessage{
						Role:      "tool_use",
						ToolName:  b.Name,
						ToolInput: inputStr,
						ToolUseID: b.ID,
						Timestamp: ev.Timestamp,
					})
				case "tool_result":
					resultText := extractContentText(b.Input)
					if len(resultText) > 500 {
						resultText = resultText[:500] + "..."
					}
					all = append(all, historyMessage{
						Role:      "tool_result",
						Content:   resultText,
						ToolUseID: b.ToolUseID,
					})
				}
			}
			if len(textParts) > 0 {
				all = append(all, historyMessage{
					Role:      "assistant",
					Content:   strings.Join(textParts, "\n"),
					Timestamp: ev.Timestamp,
				})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] history scanner error for %s: %v\n", appName, filepath.Base(path), err)
	}

	// Return only last N messages
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all
}

// subagentRecord describes a native Claude Code Agent subagent (Task) call,
// reconstructed from a session's JSONL transcript so the Web UI can backfill
// the subagent drawer when switching sessions or after a page reload.
type subagentRecord struct {
	ToolUseID    string `json:"tool_use_id"`
	Description  string `json:"description"`
	SubagentType string `json:"subagent_type"`
	Model        string `json:"model,omitempty"`
	Status       string `json:"status"`             // running | completed | error
	StartedAt    string `json:"started_at"`         // RFC3339 timestamp
	EndedAt      string `json:"ended_at,omitempty"` // RFC3339 timestamp
}

// parseSessionSubagents scans the JSONL and extracts all Agent tool_use launches
// plus their matching tool_result completions, regardless of the message limit
// applied to the chat history. The drawer needs the full lifecycle of every
// subagent ever spawned in this session so completion state is correct on
// reload / session switch.
func parseSessionSubagents(path string) []subagentRecord {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	// id → record (preserve insertion order via separate slice)
	byID := make(map[string]*subagentRecord)
	var order []string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}

		switch ev.Type {
		case "assistant":
			var blocks []struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				ID    string `json:"id"`
				Input struct {
					Description  string `json:"description"`
					SubagentType string `json:"subagent_type"`
					Model        string `json:"model"`
				} `json:"input"`
			}
			if json.Unmarshal(ev.Message.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				if b.Type != "tool_use" || b.Name != "Agent" || b.ID == "" {
					continue
				}
				if _, ok := byID[b.ID]; ok {
					continue
				}
				rec := &subagentRecord{
					ToolUseID:    b.ID,
					Description:  b.Input.Description,
					SubagentType: b.Input.SubagentType,
					Model:        b.Input.Model,
					Status:       "running",
					StartedAt:    ev.Timestamp,
				}
				if rec.SubagentType == "" {
					rec.SubagentType = "general-purpose"
				}
				if rec.Description == "" {
					rec.Description = "Agent task"
				}
				byID[b.ID] = rec
				order = append(order, b.ID)
			}
		case "user":
			// tool_result blocks live inside user content arrays
			var blocks []struct {
				Type      string          `json:"type"`
				ToolUseID string          `json:"tool_use_id"`
				IsError   bool            `json:"is_error"`
				Content   json.RawMessage `json:"content"`
			}
			if json.Unmarshal(ev.Message.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				if b.Type != "tool_result" || b.ToolUseID == "" {
					continue
				}
				rec := byID[b.ToolUseID]
				if rec == nil {
					continue
				}
				if b.IsError {
					rec.Status = "error"
				} else {
					rec.Status = "completed"
				}
				rec.EndedAt = ev.Timestamp
			}
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] subagent scanner error for %s: %v\n", appName, filepath.Base(path), err)
	}

	out := make([]subagentRecord, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

// extractImages finds base64 image blocks in user message content. Handles
// both top-level image blocks (user uploads sent directly as content) and
// images nested inside tool_result blocks (Read tool / bash screenshots).
func extractImages(raw json.RawMessage) []historyImage {
	if len(raw) == 0 {
		return nil
	}
	// Top-level blocks: either {type:"image",source:{...}} (user upload) or
	// {type:"tool_result", content:[...]} wrapping an image (tool output).
	var blocks []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
		Source  struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var images []historyImage
	for _, b := range blocks {
		switch b.Type {
		case "image":
			// User-uploaded image sent directly in the user message content
			// (produced by resolveImageBlock for ![alt](url) markdown, or by
			// native multimodal clients that post image blocks as-is).
			if b.Source.Type == "base64" && b.Source.Data != "" {
				images = append(images, historyImage{
					MediaType: b.Source.MediaType,
					Data:      b.Source.Data,
				})
			}
		case "tool_result":
			// Tool output containing images (e.g. Read of a .png, bash screenshot).
			var items []struct {
				Type   string `json:"type"`
				Source struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			}
			if json.Unmarshal(b.Content, &items) != nil {
				continue
			}
			for _, item := range items {
				if item.Type == "image" && item.Source.Type == "base64" && item.Source.Data != "" {
					images = append(images, historyImage{
						MediaType: item.Source.MediaType,
						Data:      item.Source.Data,
					})
				}
			}
		}
	}
	return images
}

// extractContentText extracts text from a JSON content field (string or array of blocks).
func extractContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// Try string first
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}

	// Try array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}
