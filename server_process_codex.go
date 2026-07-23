package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// ── Codex backend bridge → SSE / approvals (Round 4) ──
//
// This file is the codex counterpart to attachProcessBridge in
// server_session.go: it consumes the unified-event stream a *codexBackend
// emits and pumps it into the per-session sseBroadcaster the Web UI / IPC
// peers / Telegram relay all read from. It also routes codex's
// server-initiated approval requests through the existing tool-hook
// PreToolUse chain.
//
// SSE schema choices
// ------------------
//
// CC's bridge re-broadcasts the raw stream-json events (init / system /
// assistant / tool_use / tool_result / result) so the front-end already
// knows that schema. Codex events don't fit the same shape — they're a
// typed thread/turn/item stream, not a flat assistant-message dump — so
// we emit a small set of *new* event types that mirror the unified-event
// shape one-for-one:
//
//   "init"              — when the codex thread is ready (intentionally the
//                         same event name as CC so the Web UI's session-ready
//                         hint reuses the existing handler unchanged)
//   "codex_turn_started"
//   "codex_turn_completed"   (final status / error)
//   "codex_item_started"     (with kind: agent_message / tool_call / …)
//   "codex_item_delta"       (DeltaType discriminates text / output / …)
//   "codex_item_completed"
//   "codex_backend_error"
//   "ask_user_question"      (re-used from CC schema when a hook says "ask")
//   "result"                 (CC-shaped fallback for downstream consumers
//                             that already key off result for is_error /
//                             total_cost; emitted alongside codex_turn_completed)
//   "close"                  (re-used from CC schema on backend exit)
//
// Front-end work to render these natively is Round 5+; for Round 4 the
// tests + smoke confirm the events flow and the codex turn doesn't deadlock.
//
// Approval bridging
// -----------------
//
// codexBackend's handleApprovalRequest registers the request in
// pendingApprovals and emits UEvtApproval. We translate the unified
// approval into a ToolHookInput, run it through the in-process
// PreToolUse evaluator (evaluateToolHookForApproval), and call
// cb.sendPermissionDecision with the resulting allow/deny verdict. If
// the hook chain says "ask" we surface an ask_user_question SSE for the
// Web UI; the user's answer eventually flows back through the existing
// /answer-question handler, which calls sess.process.sendPermissionDecision
// — and that codepath is backend-agnostic because the Backend interface
// already has sendPermissionDecision.
//
// Default decision when no hook fires: allow. This matches CC's
// bypassPermissions behavior for tools that slipped past the harness.
// The 30s timeout in the codex backend's handleApprovalRequest is the
// safety net if the bridge dies between emit and decision.

// attachCodexBridge starts the goroutines that drain a *codexBackend's
// event stream, watch for process exit, and route approvals through the
// hook chain. Mirrors attachProcessBridge in server_session.go.
//
// source: "server-create" / "server-resume" for full init (record agent,
// sync session id), "" for reload paths.
// fullSync: true on create/resume so the session_id mapping lands in the
// DB; false on mode/model toggles which preserve the existing mapping.
func attachCodexBridge(cb *codexBackend, sess *serverSession, source string, fullSync bool) {
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		runCodexEventLoop(cb, sess, source, fullSync)
		// Backend exited: invalidate pending AUQ + cancel running tasks,
		// same as the CC bridge does after bridgeStdout returns.
		sess.dismissAllPendingAUQ("session_dead")
		if cancelled := sess.tasks.markAllRunningAsCancelled(); len(cancelled) > 0 {
			for _, t := range cancelled {
				if data, err := json.Marshal(t); err == nil {
					sess.broadcaster.broadcast(sseEvent{Event: "task_event", Data: data})
				}
			}
		}
		// Trailing close event so the front-end stops the spinner — unless
		// this exit was an intentional reload (chrome / mode / model swap).
		if !cb.suppressNextCloseFlag.Load() {
			sess.broadcaster.broadcast(sseEvent{
				Event: "close",
				Data:  []byte(`{"reason":"process_exited","backend":"codex"}`),
			})
		}
		// Clear bridgeDone if it still points at this generation's channel
		// so subsequent waitBridgeDone() doesn't return on a stale close.
		sess.mu.Lock()
		if sess.bridgeDone == doneCh {
			sess.bridgeDone = nil
		}
		sess.mu.Unlock()
	}()
	sess.mu.Lock()
	sess.bridgeDone = doneCh
	sess.mu.Unlock()
	go watchCodexExit(cb, sess)
}

// codexBridgeState carries per-session bridge state across handler calls.
// Lives only inside runCodexEventLoop / drainCodexEvents — not shared
// across sessions.
type codexBridgeState struct {
	// initEmitted gates the one-shot synthetic init emission.
	initEmitted bool
	// itemDeltaSeen tracks which item ids already produced at least one
	// CC-shaped streaming event (currently agent_message text deltas). Used
	// on UEvtItemCompleted to decide whether to re-emit the final text — if
	// deltas covered it, we skip to avoid the typewriter showing the message
	// twice. Bounded by the number of items in a turn (small).
	itemDeltaSeen map[string]bool
}

func newCodexBridgeState() *codexBridgeState {
	return &codexBridgeState{itemDeltaSeen: map[string]bool{}}
}

// runCodexEventLoop is the synchronous body of the bridge goroutine. Reads
// events off cb.events() until the channel is closed (on shutdown) or the
// backend dies (cb.done fires).
func runCodexEventLoop(cb *codexBackend, sess *serverSession, source string, fullSync bool) {
	// Synthetic init: emit as soon as the backend reports its thread id so
	// downstream consumers know the session is live. Matches CC's init
	// emission timing — driven off the backend's first useful event.
	state := newCodexBridgeState()

	for {
		select {
		case ev := <-cb.events():
			// eventsCh is never closed by the producer; backend exit is
			// signaled exclusively via cb.done. No `!ok` check needed.
			handleCodexUnifiedEvent(cb, sess, ev, state, source, fullSync)
		case <-cb.done:
			// Drain any events the producer wrote before close so the front-end
			// sees the final turn_completed / error before close arrives. Done
			// non-blockingly: if no events queued we exit immediately.
			drainCodexEvents(cb, sess, state, source, fullSync)
			return
		}
	}
}

// drainCodexEvents flushes whatever is left in cb.events() after backend
// exit. Bounded by the channel capacity (codexEventsChanCapacity, currently
// 256 events) so it terminates naturally. The hard maxDrainIters cap is
// defensive paranoia for a future where someone changes the channel from
// buffered to a producer that keeps shoveling — turning this into the
// shutdown hot path is much worse than dropping a few stragglers.
func drainCodexEvents(cb *codexBackend, sess *serverSession, state *codexBridgeState, source string, fullSync bool) {
	const maxDrainIters = 10000
	for i := 0; i < maxDrainIters; i++ {
		select {
		case ev := <-cb.events():
			// eventsCh is never closed; we drain whatever is buffered then
			// exit on the default branch when empty.
			handleCodexUnifiedEvent(cb, sess, ev, state, source, fullSync)
		default:
			return
		}
	}
	fmt.Fprintf(os.Stderr, "[%s] codex: drainCodexEvents hit max iter cap (%d) — possible producer leak\n",
		appName, maxDrainIters)
}

// handleCodexUnifiedEvent converts one UnifiedEvent into the matching SSE
// emissions and (for approvals) drives the tool-hook chain.
func handleCodexUnifiedEvent(cb *codexBackend, sess *serverSession, ev UnifiedEvent, state *codexBridgeState, source string, fullSync bool) {
	// First useful event triggers our synthetic init. Codex doesn't have a
	// dedicated "I'm ready, here are my tools" message after thread/start,
	// so we synthesize one when the first turn or first item lands. This
	// gives the Web UI's "session ready" hint a stable signal.
	if !state.initEmitted && (ev.Kind == UEvtTurnStarted || ev.Kind == UEvtItemStarted) {
		state.initEmitted = true
		emitCodexSyntheticInit(cb, sess, source, fullSync)
	}

	switch ev.Kind {
	case UEvtTurnStarted:
		sess.broadcaster.broadcast(sseEvent{Event: "codex_turn_started", Data: codexEventEnvelope(ev)})
		sess.touch()

	case UEvtTurnCompleted:
		// Mirror CC: turn_completed bumps NumTurns / cost and transitions
		// the session back to idle. Without this the Web UI's per-session
		// turn counter and the auto-rename trigger never advance — that's
		// the user-visible "session won't start" symptom.
		var payload UnifiedTurnPayload
		_ = json.Unmarshal(ev.Payload, &payload)
		newStatus := "idle"
		if payload.Status == "error" {
			newStatus = "error"
		}
		sess.mu.Lock()
		// NumTurns counts user-visible turns: codex emits one
		// turn_completed per user input regardless of how many internal
		// model calls the turn made, so a single ++ is the right unit.
		// (CC's path adds result.NumTurns because CC reports cumulative
		// turns inside one stream-json result; codex doesn't.)
		sess.NumTurns++
		if payload.CostUSD > 0 {
			sess.TotalCost += payload.CostUSD
		}
		sess.mu.Unlock()
		// Status flip before the SSE result line so any consumer that
		// reads status when it sees "result" observes the new value.
		sess.setStatus(newStatus)
		if fullSync {
			// DB persistence is best-effort and runs off the bridge goroutine
			// to keep the event loop snappy. sess.ID is immutable so we can
			// close over it directly without copying.
			go func() { _, _ = incrementUserTurns(sess.ID) }()
		}
		// Emit both the typed codex event AND a CC-shaped "result" so any
		// downstream consumer that keys off result for is_error / status
		// keeps working.
		sess.broadcaster.broadcast(sseEvent{Event: "codex_turn_completed", Data: codexEventEnvelope(ev)})
		sess.broadcaster.broadcast(sseEvent{Event: "result", Data: codexResultEnvelope(ev)})
		// Turn boundary: forget per-item delta state. Any straggler items
		// from this turn that arrive after turn_completed (shouldn't, but
		// defense in depth) get treated as if no delta was seen.
		state.itemDeltaSeen = map[string]bool{}

	case UEvtItemStarted:
		sess.broadcaster.broadcast(sseEvent{Event: "codex_item_started", Data: codexEventEnvelope(ev)})

	case UEvtItemDelta:
		sess.broadcaster.broadcast(sseEvent{Event: "codex_item_delta", Data: codexEventEnvelope(ev)})
		// Round 5: also emit a CC-shaped streaming event so the existing
		// Web UI typewriter (handleAssistant) renders codex text live —
		// without this, agent_message text only shows up after a refresh
		// because /api/history reads the JSONL persisted on item_completed.
		broadcastCodexDeltaAsCC(cb, sess, ev, state)

	case UEvtItemCompleted:
		sess.broadcaster.broadcast(sseEvent{Event: "codex_item_completed", Data: codexEventEnvelope(ev)})
		// Persist the item to the session JSONL so /api/history/{id}/messages,
		// FTS, and the rename worker all see it. Schema mirrors CC's
		// transcript so we reuse parseSessionMessages unchanged.
		persistCodexItemCompleted(cb, sess, ev)
		// Round 5: re-broadcast as CC-shaped tool_use/tool_result/assistant
		// so the Web UI's existing handlers render the tool chain live.
		// Previous behavior only surfaced these on page refresh because the
		// UI's switch had no case for codex_item_*.
		broadcastCodexItemCompletedAsCC(cb, sess, ev, state)

	case UEvtBackendError:
		sess.broadcaster.broadcast(sseEvent{Event: "codex_backend_error", Data: codexEventEnvelope(ev)})

	case UEvtApproval:
		go handleCodexApproval(cb, sess, ev)
	}
}

// emitCodexSyntheticInit broadcasts a CC-shaped init event so the front-end
// sees a familiar "init" payload. We embed the codex-specific fields
// (backend kind, thread id) under "codex" so future Web UI work can render
// a backend badge.
func emitCodexSyntheticInit(cb *codexBackend, sess *serverSession, source string, fullSync bool) {
	info := cb.info()
	msg := map[string]any{
		"type":           "system",
		"subtype":        "init",
		"cwd":            cb.cwd,
		"session_id":     info.SessionID,
		"model":          info.Model,
		"tools":          []string{},
		"mcp_servers":    []map[string]any{},
		"permissionMode": "bypassPermissions",
		"backend": map[string]any{
			"kind":  string(info.Kind),
			"model": info.Model,
		},
	}
	raw, _ := json.Marshal(msg)
	sess.broadcaster.broadcast(sseEvent{Event: "init", Data: raw})

	// On full-sync paths (server-create / server-resume) record the
	// session id mapping so resume / IPC peers can find the session by
	// codex-side id. Mirrors CC's setClaudeSessionID + recordSessionAgent
	// in makeOnInit.
	if fullSync && info.SessionID != "" {
		sess.mu.Lock()
		sess.ClaudeSID = info.SessionID
		hub := sess.hub
		sess.mu.Unlock()
		setClaudeSessionID(sess.ID, info.SessionID)
		recordSessionAgent(info.SessionID, "main", appName, source)
		if hub != nil {
			hub.notifySessions()
		}
	}
	// Seed the session JSONL with a CC-shaped system/init line so
	// readClaudeSessionName / parseSessionMessages can recognize the file
	// the same way they recognize a Claude Code transcript.
	writeCodexSystemInit(info.SessionID, info.Model, cb.cwd)
}

// codexEventEnvelope serializes one UnifiedEvent for SSE transport. We
// hand the whole envelope through (kind/turn/item/payload/raw) so the
// front-end has full fidelity for any future renderer.
func codexEventEnvelope(ev UnifiedEvent) []byte {
	raw, _ := json.Marshal(ev)
	return raw
}

// codexResultEnvelope produces a CC-shaped result message for the browser's
// existing result-event handler. Only the fields downstream consumers
// actually read (subtype, is_error, total_cost_usd) are populated; codex
// doesn't currently expose per-turn cost so it's left at 0 for now.
func codexResultEnvelope(ev UnifiedEvent) []byte {
	var payload UnifiedTurnPayload
	_ = json.Unmarshal(ev.Payload, &payload)
	subtype := "success"
	isErr := false
	if payload.Status == "error" {
		subtype = "error"
		isErr = true
	} else if payload.Status == "cancelled" {
		subtype = "cancelled"
	}
	out := map[string]any{
		"type":           "result",
		"subtype":        subtype,
		"is_error":       isErr,
		"result":         payload.Error,
		"session_id":     ev.TurnID,
		"total_cost_usd": payload.CostUSD,
		"backend":        "codex",
	}
	raw, _ := json.Marshal(out)
	return raw
}

// handleCodexApproval is invoked once per UEvtApproval. It synthesizes a
// PreToolUse hook payload, runs the in-process evaluator, and replies via
// cb.sendPermissionDecision. If the hook chain says "ask" we surface an
// ask_user_question SSE for the Web UI and let the user's answer arrive
// later through the existing /answer-question endpoint (which calls
// sess.process.sendPermissionDecision — backend-agnostic).
func handleCodexApproval(cb *codexBackend, sess *serverSession, ev UnifiedEvent) {
	var ua UnifiedApproval
	if err := json.Unmarshal(ev.Payload, &ua); err != nil {
		// Malformed payload — default-allow to avoid deadlocking codex.
		// The 30s timeout would catch this anyway but we don't want to
		// burn that budget on a programmer error.
		_ = cb.sendPermissionDecision(ua.RequestID, map[string]any{
			"behavior": "allow",
			"message":  "weiran: malformed approval payload, allowing",
		})
		return
	}

	// Synthesize a PreToolUse hook input. session_id is the weiran session
	// id (not the codex thread id) so tool-hook rules that pivot on
	// session_id (e.g. mark_restart_initiator) keep working.
	hookIn := ToolHookInput{
		SessionID:     sess.ID,
		CWD:           sess.Project,
		ToolName:      ua.ToolName,
		ToolInput:     ua.Input,
		HookEventName: HookEventPreToolUse,
	}

	decision, reason, contexts := evaluateToolHookForApproval(hookIn)
	recordCodexApprovalDecision(decision)
	switch decision {
	case "deny":
		msg := reason
		if msg == "" {
			msg = "weiran tool-hook: denied by rule"
		}
		_ = cb.sendPermissionDecision(ua.RequestID, map[string]any{
			"behavior": "deny",
			"message":  msg,
		})
		// Surface the deny reason on SSE so the operator sees why.
		emitCodexHookDecision(sess, ua, "deny", msg, contexts)

	case "ask":
		// Mirror CC's permission-prompt path: record a pendingAUQEntry so
		// /answer-question can reply later, broadcast ask_user_question.
		entry := &pendingAUQEntry{
			RequestID: ua.RequestID,
			ToolUseID: ua.ToolUseID,
			Input:     ua.Input,
			CreatedAt: time.Now(),
			Kind:      "permission",
			ToolName:  ua.ToolName,
		}
		sess.recordPendingAUQ(entry)
		synthetic, _ := json.Marshal(map[string]any{
			"request_id":  ua.RequestID,
			"tool_use_id": ua.ToolUseID,
			"kind":        "permission",
			"tool_name":   ua.ToolName,
			"input":       synthesizePermissionAUQInput(ua.ToolName, ua.Input),
			"backend":     "codex",
			"hook_reason": reason,
		})
		sess.broadcaster.broadcast(sseEvent{Event: "ask_user_question", Data: synthetic})
		// Don't send a decision here — it'll come from /answer-question.

	case "allow":
		_ = cb.sendPermissionDecision(ua.RequestID, map[string]any{
			"behavior": "allow",
			"message":  reason,
		})
		emitCodexHookDecision(sess, ua, "allow", reason, contexts)

	default:
		// No matching rule — default-allow (matches CC's bypassPermissions).
		_ = cb.sendPermissionDecision(ua.RequestID, map[string]any{
			"behavior": "allow",
			"message":  "weiran: no matching tool-hook rule, default allow",
		})
	}
}

// emitCodexHookDecision broadcasts a small SSE event so the operator can
// see when the hook chain auto-decided an approval. Useful for Round 4
// observability; harmless to ignore on the Web UI side.
func emitCodexHookDecision(sess *serverSession, ua UnifiedApproval, decision, reason string, contexts []string) {
	if reason == "" && len(contexts) == 0 {
		return
	}
	out := map[string]any{
		"request_id":  ua.RequestID,
		"tool_name":   ua.ToolName,
		"tool_use_id": ua.ToolUseID,
		"decision":    decision,
		"reason":      reason,
		"contexts":    contexts,
	}
	raw, _ := json.Marshal(out)
	sess.broadcaster.broadcast(sseEvent{Event: "codex_hook_decision", Data: raw})
}

// watchCodexExit is the codex counterpart to watchExit in server_session.go.
// Blocks on cb.done, then transitions the session to stopped/error and (for
// ephemeral sessions) optionally retries with the next fallback model.
//
// Round 4 keeps fallback minimal — codex sessions don't currently chain
// into the CC-style fallbackModels list. If a codex backend exits with
// rate-limit, we mark the session error and let the operator retry. Round
// 5 can extend this to share the CC retry path if desired.
func watchCodexExit(cb *codexBackend, sess *serverSession) {
	<-cb.done
	sess.mu.Lock()
	alreadyStopped := sess.Status == "stopped"
	sess.mu.Unlock()
	if alreadyStopped {
		return
	}

	if errPtr := cb.initErr.Load(); errPtr != nil && *errPtr != "" {
		fmt.Fprintf(os.Stderr, "[%s] server: codex session %s init error: %s\n",
			appName, shortID(sess.ID), *errPtr)
		sess.setStatus("error")
		return
	}
	if exitCode := cb.exitCode.Load(); exitCode != 0 || cb.rateLimited.Load() {
		fmt.Fprintf(os.Stderr, "[%s] server: codex session %s exited code=%d rate_limited=%v\n",
			appName, shortID(sess.ID), exitCode, cb.rateLimited.Load())
		sess.setStatus("error")
		return
	}
	sess.setStatus("stopped")
}

// persistCodexItemCompleted mirrors a finished codex item into the session
// JSONL so the existing Web UI / FTS / rename code paths reuse the same
// transcript layer they use for CC. Item kind controls the content shape:
//
//   - agent_message → assistant message with one text block
//   - reasoning     → assistant message with one thinking block
//   - tool_call / command_exec / file_change → assistant tool_use block
//     followed by a synthesized user tool_result so parseSessionMessages
//     can pair them up like a CC transcript.
//
// Anything we don't recognize is dropped silently — the SSE bridge still
// surfaced it via codex_item_completed for live consumers.
func persistCodexItemCompleted(cb *codexBackend, sess *serverSession, ev UnifiedEvent) {
	// Snapshot ClaudeSID under sess.mu — the bridge goroutine runs in
	// parallel with emitCodexSyntheticInit (which writes ClaudeSID under
	// the same lock) and other writers in server_session.go. Reading the
	// field unlocked is a data race the test -race detector flags.
	sess.mu.Lock()
	threadID := sess.ClaudeSID
	sess.mu.Unlock()
	if threadID == "" {
		// Fall back to the live backend info — happens only during the
		// brief window between handshake and setClaudeSessionID.
		threadID = cb.info().SessionID
	}
	if threadID == "" {
		return
	}
	model := cb.info().Model

	var payload UnifiedItemPayload
	_ = json.Unmarshal(ev.Payload, &payload)
	resultText := extractCodexResultText(payload.Result)

	switch ev.ItemKind {
	case UItemAgentMessage:
		if resultText != "" {
			writeCodexAssistantMessage(threadID, model, resultText)
		}

	case UItemReasoning:
		if resultText != "" {
			writeCodexAssistantBlock(threadID, model, map[string]any{
				"type":      "thinking",
				"thinking":  resultText,
				"signature": "",
			})
		}

	case UItemToolCall, UItemCommandExec, UItemFileChange:
		toolName := payload.Name
		if toolName == "" {
			toolName = string(ev.ItemKind)
		}
		var input any
		if len(payload.Input) > 0 {
			_ = json.Unmarshal(payload.Input, &input)
		}
		writeCodexAssistantBlock(threadID, model, map[string]any{
			"type":  "tool_use",
			"id":    ev.ItemID,
			"name":  toolName,
			"input": input,
		})
		// Pair with a tool_result so the existing CC parser doesn't see a
		// dangling tool_use. resultText is already the human-readable
		// aggregated stdout for command_exec / final patch text for
		// file_change / generic result for tool_call.
		writeCodexToolResult(threadID, ev.ItemID, resultText, payload.IsError)
	}
}

// extractCodexResultText pulls the human-readable text out of a codex item
// result blob. The codex bridge wraps text-bearing items in
// {"text": "..."}, but file_change and tool_call may carry a raw object
// (e.g. a list of changes). We try the common shapes and fall back to the
// raw JSON string so the transcript at least preserves *something*.
func extractCodexResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var wrapped struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && wrapped.Text != "" {
		return wrapped.Text
	}
	// Some kinds (file_change / dynamic_tool_call) hand back the verbatim
	// codex payload. Stringify it so it lands in FTS as one searchable
	// blob instead of disappearing.
	return string(raw)
}

// ── CC-shaped re-broadcast (Round 5) ─────────────────────────────────────
//
// The Web UI's handleSessionEvent switch only has cases for the CC
// stream-json schema (assistant / tool_use / tool_result / result). Codex
// events arrive as codex_item_started / codex_item_delta /
// codex_item_completed which the UI silently drops — that's the user-
// visible "tool chain doesn't render until I refresh" symptom. The fix is
// purely additive: alongside the typed codex_* events we emit the same
// content in CC schema, so the existing handleAssistant / handleToolUse /
// handleToolResult render codex output live, and any downstream consumer
// (Telegram relay, IPC peer) that already keys off CC schema also wakes up.
//
// Translation rules
// -----------------
//
//   UEvtItemDelta  ItemKind=agent_message  DeltaType=text
//       → SSE "assistant" with content=[{type:"text", text:<delta>}]
//         The UI's typewriter (typeBuf+=text) accumulates these correctly.
//         Mark itemDeltaSeen[ItemID]=true so completion doesn't re-emit.
//
//   UEvtItemDelta  other (reasoning text, plan, output, summary)
//       → no CC re-broadcast. The Web UI doesn't have a streaming handler
//         for thinking/plan/output blocks; refresh + JSONL still captures
//         them. Adding a renderer is future work.
//
//   UEvtItemCompleted  ItemKind=agent_message
//       → if itemDeltaSeen[ItemID] already true, skip (deltas covered it;
//         re-emitting would duplicate text in the typewriter).
//         Otherwise emit "assistant" with the final text in one shot —
//         this covers backends/turns where codex didn't stream deltas.
//
//   UEvtItemCompleted  ItemKind=reasoning
//       → no CC re-broadcast. handleAssistant ignores thinking blocks
//         (only renders type=="text"). JSONL persistence still captures it.
//
//   UEvtItemCompleted  ItemKind=tool_call|command_exec|file_change
//       → emit "tool_use" (assistant message wrapping a tool_use block)
//         followed by "tool_result" (user message wrapping a tool_result
//         block). Same shape persistCodexItemCompleted writes to JSONL —
//         we just publish it on the wire too.

// broadcastCodexDeltaAsCC emits a CC-shaped streaming event for codex
// item deltas the Web UI knows how to render. Called *after* the typed
// codex_item_delta event, so any consumer that prefers the typed shape
// still sees it first; consumers that prefer CC schema see both.
func broadcastCodexDeltaAsCC(cb *codexBackend, sess *serverSession, ev UnifiedEvent, state *codexBridgeState) {
	// Only agent_message text deltas translate cleanly to the UI's
	// typewriter today. Everything else (reasoning text, plan, output,
	// summary) needs a richer renderer the UI doesn't have yet — leave
	// those to JSONL/refresh until a Web UI follow-up adds handlers.
	if ev.ItemKind != UItemAgentMessage || ev.DeltaType != "text" {
		return
	}
	var payload UnifiedDeltaPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil || payload.Text == "" {
		return
	}
	model := cb.info().Model
	msg := map[string]any{
		"type":      "assistant",
		"timestamp": nowRFC3339(),
		"message": map[string]any{
			"role":  "assistant",
			"model": model,
			"content": []map[string]any{
				{"type": "text", "text": payload.Text},
			},
		},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return
	}
	sess.broadcaster.broadcast(sseEvent{Event: "assistant", Data: raw})
	if ev.ItemID != "" {
		state.itemDeltaSeen[ev.ItemID] = true
	}
}

// broadcastCodexItemCompletedAsCC emits CC-shaped events for an item that
// just finished. Mirror of the persist function but pushes through the SSE
// broadcaster instead of the JSONL file. See the rules block above for how
// each ItemKind maps to wire events.
func broadcastCodexItemCompletedAsCC(cb *codexBackend, sess *serverSession, ev UnifiedEvent, state *codexBridgeState) {
	var payload UnifiedItemPayload
	_ = json.Unmarshal(ev.Payload, &payload)
	resultText := extractCodexResultText(payload.Result)
	model := cb.info().Model
	timestamp := nowRFC3339()

	switch ev.ItemKind {
	case UItemAgentMessage:
		// Streaming deltas already painted the message — don't double-paint.
		if ev.ItemID != "" && state.itemDeltaSeen[ev.ItemID] {
			delete(state.itemDeltaSeen, ev.ItemID)
			return
		}
		if resultText == "" {
			return
		}
		msg := map[string]any{
			"type":      "assistant",
			"timestamp": timestamp,
			"message": map[string]any{
				"role":  "assistant",
				"model": model,
				"content": []map[string]any{
					{"type": "text", "text": resultText},
				},
			},
		}
		raw, err := json.Marshal(msg)
		if err != nil {
			return
		}
		sess.broadcaster.broadcast(sseEvent{Event: "assistant", Data: raw})

	case UItemToolCall, UItemCommandExec, UItemFileChange:
		toolName := payload.Name
		if toolName == "" {
			toolName = string(ev.ItemKind)
		}
		var input any
		if len(payload.Input) > 0 {
			_ = json.Unmarshal(payload.Input, &input)
		}
		toolUseID := ev.ItemID
		// 1) tool_use — assistant message wrapping the tool_use block.
		toolUseMsg := map[string]any{
			"type":      "assistant",
			"timestamp": timestamp,
			"message": map[string]any{
				"role":  "assistant",
				"model": model,
				"content": []map[string]any{
					{
						"type":  "tool_use",
						"id":    toolUseID,
						"name":  toolName,
						"input": input,
					},
				},
			},
		}
		if raw, err := json.Marshal(toolUseMsg); err == nil {
			sess.broadcaster.broadcast(sseEvent{Event: "tool_use", Data: raw})
		}
		// 2) tool_result — paired user message so the UI can pair them
		// with the same tool_use_id. Skip when there's no tool_use_id —
		// the UI keys off it for pairing and an empty id would orphan.
		if toolUseID == "" {
			return
		}
		resultBlock := map[string]any{
			"type":        "tool_result",
			"tool_use_id": toolUseID,
			"content":     resultText,
		}
		if payload.IsError {
			resultBlock["is_error"] = true
		}
		toolResultMsg := map[string]any{
			"type":      "user",
			"timestamp": timestamp,
			"message": map[string]any{
				"role":    "user",
				"content": []map[string]any{resultBlock},
			},
		}
		if raw, err := json.Marshal(toolResultMsg); err == nil {
			sess.broadcaster.broadcast(sseEvent{Event: "tool_result", Data: raw})
		}

	case UItemReasoning:
		// Web UI's handleAssistant filters to type=="text", so emitting a
		// thinking block here would be silently dropped. JSONL persistence
		// already captures it for refresh/history. Skip for now; a future
		// renderer can read codex_item_completed directly to show a
		// collapsed "thinking…" panel inline.
		return
	}
}
