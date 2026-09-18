package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ── Codex bridge tests (Round 4) ──
//
// These tests validate the UnifiedEvent → SSE translation in
// server_process_codex.go without spawning a real codex process. We build
// a *codexBackend by hand (no fake-server harness needed because the
// bridge consumes from cb.events() / cb.done — both controllable from
// test-side), wire it to a real *serverSession + sseBroadcaster, push
// events, and assert on the SSE stream.

// newCodexBridgeTestSession builds a minimal serverSession+codexBackend pair
// suitable for bridge tests. The codex backend has no underlying process —
// we drive its eventsCh directly.
func newCodexBridgeTestSession(t *testing.T) (*serverSession, *codexBackend, *subscriber) {
	t.Helper()
	// Sandbox appDir so emitCodexSyntheticInit's JSONL write doesn't escape
	// into the developer's real data directory.
	origAppDir := appDir
	appDir = t.TempDir()
	t.Cleanup(func() { appDir = origAppDir })
	bc := newBroadcaster()
	sess := &serverSession{
		ID:          "test-session",
		Project:     "/tmp",
		broadcaster: bc,
		Backend:     BackendCodex,
		tasks:       newTaskTracker(),
	}
	cb := newCodexBackend(SessionOpts{Model: "gpt-test"})
	// Pre-populate thread id so emitCodexSyntheticInit has a session_id to
	// emit. Also signal init so anything that calls waitInit returns true.
	thr := "thr_test"
	cb.threadID.Store(&thr)
	cb.signalInit()

	sub := bc.subscribe()
	t.Cleanup(func() {
		bc.unsubscribe(sub)
		cb.markDone()
		sess.waitBridgeDone()
	})
	return sess, cb, sub
}

// readSSEEvents drains up to maxEvents events from a subscriber within
// the given timeout. Returns the events seen so far on timeout.
func readSSEEvents(t *testing.T, sub *subscriber, maxEvents int, timeout time.Duration) []sseEvent {
	t.Helper()
	deadline := time.After(timeout)
	out := make([]sseEvent, 0, maxEvents)
	for len(out) < maxEvents {
		select {
		case ev := <-sub.ch:
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
	return out
}

func TestAttachCodexBridge_TurnFlowEmitsSSE(t *testing.T) {
	sess, cb, sub := newCodexBridgeTestSession(t)

	attachCodexBridge(cb, sess, "server-create", false)

	// Push: turn_started → item_started (agent_message) → item_delta → item_completed → turn_completed.
	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnStarted,
		TurnID:  "turn-1",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "running"}),
	})
	cb.emit(UnifiedEvent{
		Kind:     UEvtItemStarted,
		TurnID:   "turn-1",
		ItemID:   "item-1",
		ItemKind: UItemAgentMessage,
		Payload:  mustMarshalRaw(UnifiedItemPayload{}),
	})
	cb.emit(UnifiedEvent{
		Kind:      UEvtItemDelta,
		TurnID:    "turn-1",
		ItemID:    "item-1",
		ItemKind:  UItemAgentMessage,
		DeltaType: "text",
		Payload:   mustMarshalRaw(UnifiedDeltaPayload{Text: "Hi"}),
	})
	cb.emit(UnifiedEvent{
		Kind:     UEvtItemCompleted,
		TurnID:   "turn-1",
		ItemID:   "item-1",
		ItemKind: UItemAgentMessage,
		Payload:  mustMarshalRaw(UnifiedItemPayload{}),
	})
	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnCompleted,
		TurnID:  "turn-1",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "ok"}),
	})

	// Expected SSE events (in order):
	//   init (synthetic, fires on first useful event)
	//   codex_turn_started
	//   codex_item_started
	//   codex_item_delta
	//   assistant   (Round 5: CC-shaped re-broadcast of the text delta)
	//   codex_item_completed
	//   codex_turn_completed
	//   result   (CC-shaped fallback alongside turn_completed)
	//
	// Note: codex_item_completed for an agent_message whose deltas already
	// rendered does NOT re-emit a CC "assistant" event (avoids typewriter
	// double-paint). See broadcastCodexItemCompletedAsCC.
	want := []string{
		"init",
		"codex_turn_started",
		"codex_item_started",
		"codex_item_delta",
		"assistant",
		"codex_item_completed",
		"codex_turn_completed",
		"result",
	}
	got := readSSEEvents(t, sub, len(want), 2*time.Second)
	if len(got) < len(want) {
		t.Fatalf("got %d events, want %d: %v", len(got), len(want), eventNames(got))
	}
	for i, w := range want {
		if got[i].Event != w {
			t.Errorf("event[%d] = %q, want %q (full sequence: %v)", i, got[i].Event, w, eventNames(got))
		}
	}

	// The CC-shaped assistant event should carry exactly the streamed
	// delta text — the UI's typewriter relies on appending each delta as
	// it arrives. Re-emitting the full message would double-paint.
	var asstPayload map[string]any
	if err := json.Unmarshal(got[4].Data, &asstPayload); err != nil {
		t.Fatalf("decode assistant payload: %v", err)
	}
	if asstPayload["type"] != "assistant" {
		t.Errorf("assistant payload type = %v, want assistant", asstPayload["type"])
	}
	msg, _ := asstPayload["message"].(map[string]any)
	contents, _ := msg["content"].([]any)
	if len(contents) != 1 {
		t.Fatalf("assistant content len = %d, want 1: %v", len(contents), contents)
	}
	first, _ := contents[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "Hi" {
		t.Errorf("assistant content[0] = %v, want {type:text, text:Hi}", first)
	}

	// Verify the synthetic init carries backend=codex and session_id=thread id.
	var initPayload map[string]any
	if err := json.Unmarshal(got[0].Data, &initPayload); err != nil {
		t.Fatalf("decode init payload: %v", err)
	}
	if initPayload["session_id"] != "thr_test" {
		t.Errorf("init session_id = %v, want thr_test", initPayload["session_id"])
	}
	backendField, _ := initPayload["backend"].(map[string]any)
	if backendField["kind"] != string(BackendCodex) {
		t.Errorf("init backend.kind = %v, want codex", backendField["kind"])
	}

	// Verify the result envelope carries subtype=success on a clean turn.
	// Index shifted by 1 from Round 4 because Round 5 inserts a CC-shaped
	// "assistant" event between codex_item_delta and codex_item_completed.
	var resultPayload map[string]any
	if err := json.Unmarshal(got[7].Data, &resultPayload); err != nil {
		t.Fatalf("decode result payload: %v", err)
	}
	if resultPayload["subtype"] != "success" {
		t.Errorf("result subtype = %v, want success", resultPayload["subtype"])
	}
	if resultPayload["backend"] != "codex" {
		t.Errorf("result backend = %v, want codex", resultPayload["backend"])
	}

	// Session should be in idle status after turn_completed.
	sess.mu.Lock()
	gotStatus := sess.Status
	sess.mu.Unlock()
	if gotStatus != "idle" {
		t.Errorf("session status = %q, want idle", gotStatus)
	}
}

func TestAttachCodexBridge_TurnErrorFlipsStatusError(t *testing.T) {
	sess, cb, sub := newCodexBridgeTestSession(t)
	attachCodexBridge(cb, sess, "server-create", false)

	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnStarted,
		TurnID:  "turn-err",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "running"}),
	})
	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnCompleted,
		TurnID:  "turn-err",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "error", Error: "boom"}),
	})

	// init + codex_turn_started + codex_turn_completed + result
	got := readSSEEvents(t, sub, 4, 2*time.Second)
	if len(got) < 4 {
		t.Fatalf("got %d events, want 4: %v", len(got), eventNames(got))
	}

	// Result envelope must reflect the error.
	resultEv := got[3]
	if resultEv.Event != "result" {
		t.Fatalf("event[3] = %q, want result", resultEv.Event)
	}
	var resultPayload map[string]any
	_ = json.Unmarshal(resultEv.Data, &resultPayload)
	if resultPayload["is_error"] != true {
		t.Errorf("result is_error = %v, want true", resultPayload["is_error"])
	}
	if resultPayload["subtype"] != "error" {
		t.Errorf("result subtype = %v, want error", resultPayload["subtype"])
	}
	if resultPayload["result"] != "boom" {
		t.Errorf("result message = %v, want boom", resultPayload["result"])
	}

	sess.mu.Lock()
	gotStatus := sess.Status
	sess.mu.Unlock()
	if gotStatus != "error" {
		t.Errorf("session status = %q, want error", gotStatus)
	}
}

func TestAttachCodexBridge_BackendErrorEvent(t *testing.T) {
	sess, cb, sub := newCodexBridgeTestSession(t)
	attachCodexBridge(cb, sess, "server-create", false)

	// First push a turn_started so the synthetic init fires; otherwise the
	// backend_error alone wouldn't trip the "first useful event" gate.
	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnStarted,
		TurnID:  "turn-z",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "running"}),
	})
	cb.emit(UnifiedEvent{
		Kind: UEvtBackendError,
		Payload: mustMarshalRaw(map[string]any{
			"reason": "transport closed",
		}),
	})

	got := readSSEEvents(t, sub, 3, 2*time.Second)
	if len(got) < 3 {
		t.Fatalf("got %d events, want 3: %v", len(got), eventNames(got))
	}
	if got[2].Event != "codex_backend_error" {
		t.Errorf("event[2] = %q, want codex_backend_error", got[2].Event)
	}
}

func TestAttachCodexBridge_CloseEventOnExit(t *testing.T) {
	sess, cb, sub := newCodexBridgeTestSession(t)
	attachCodexBridge(cb, sess, "server-create", false)

	// Mark backend done — bridge should drain remaining events and then
	// emit a close SSE event.
	cb.markDone()

	// Allow bridge to process exit + emit close.
	got := readSSEEvents(t, sub, 1, 2*time.Second)
	if len(got) == 0 {
		t.Fatal("no SSE event observed after backend exit")
	}
	// The first (and likely only) event should be close.
	foundClose := false
	for _, ev := range got {
		if ev.Event == "close" {
			foundClose = true
			if !strings.Contains(string(ev.Data), `"backend":"codex"`) {
				t.Errorf("close payload missing backend=codex: %s", ev.Data)
			}
		}
	}
	if !foundClose {
		t.Errorf("never saw close event; got: %v", eventNames(got))
	}
}

func TestAttachCodexBridge_SuppressNextCloseSkipsClose(t *testing.T) {
	sess, cb, sub := newCodexBridgeTestSession(t)
	cb.suppressNextClose()
	attachCodexBridge(cb, sess, "server-create", false)
	cb.markDone()

	// Wait briefly to let the bridge run and verify no close event arrives.
	got := readSSEEvents(t, sub, 1, 500*time.Millisecond)
	for _, ev := range got {
		if ev.Event == "close" {
			t.Errorf("close event fired despite suppressNextClose: %s", ev.Data)
		}
	}
}

func TestAttachCodexBridge_ApprovalDefaultAllowsWhenNoRule(t *testing.T) {
	sess, cb, _ := newCodexBridgeTestSession(t)
	// Override timeout per-instance to keep the test fast.
	cb.approvalWaitTimeout = 100 * time.Millisecond
	attachCodexBridge(cb, sess, "server-create", false)

	// Manually register a pending approval (simulates handleApprovalRequest
	// without spinning a fake JSON-RPC server). We then emit UEvtApproval
	// and expect the bridge's handleCodexApproval goroutine to call
	// sendPermissionDecision(allow), which removes the entry.
	approvalID := "approval-test-1"
	pending := &pendingCodexApproval{
		method: ServerReqCommandExecApproval,
		reply:  make(chan map[string]any, 1),
	}
	cb.approvalsMu.Lock()
	cb.pendingApprovals[approvalID] = pending
	cb.approvalsMu.Unlock()

	cb.emit(UnifiedEvent{
		Kind: UEvtApproval,
		Payload: mustMarshalRaw(UnifiedApproval{
			RequestID: approvalID,
			ToolName:  "Bash",
			Input:     json.RawMessage(`{"command":"ls"}`),
			Subtype:   "permission",
		}),
	})

	// Wait for the bridge to wake and decide.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case decision := <-pending.reply:
			if decision["behavior"] != "allow" {
				t.Errorf("default decision behavior = %v, want allow", decision["behavior"])
			}
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("approval bridge never resolved")
}

// TestAttachCodexBridge_ToolCallEmitsCCToolUseAndResult validates the
// Round 5 CC-shape re-broadcast for tool kinds: a UEvtItemCompleted
// item_kind=tool_call should produce one tool_use event AND one tool_result
// event in CC stream-json schema, alongside the typed codex_item_completed.
// Without these, the Web UI's tool chain only renders after a refresh.
func TestAttachCodexBridge_ToolCallEmitsCCToolUseAndResult(t *testing.T) {
	sess, cb, sub := newCodexBridgeTestSession(t)
	attachCodexBridge(cb, sess, "server-create", false)

	// Push a turn with a single tool_call item (no deltas — most tools
	// surface only on completion).
	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnStarted,
		TurnID:  "t1",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "running"}),
	})
	cb.emit(UnifiedEvent{
		Kind:     UEvtItemStarted,
		TurnID:   "t1",
		ItemID:   "tool-1",
		ItemKind: UItemToolCall,
		Payload: mustMarshalRaw(UnifiedItemPayload{
			Name:  "Bash",
			Input: json.RawMessage(`{"command":"ls"}`),
		}),
	})
	cb.emit(UnifiedEvent{
		Kind:     UEvtItemCompleted,
		TurnID:   "t1",
		ItemID:   "tool-1",
		ItemKind: UItemToolCall,
		Payload: mustMarshalRaw(UnifiedItemPayload{
			Name:    "Bash",
			Input:   json.RawMessage(`{"command":"ls"}`),
			Result:  json.RawMessage(`{"text":"file-a\nfile-b"}`),
			IsError: false,
		}),
	})
	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnCompleted,
		TurnID:  "t1",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "ok"}),
	})

	// Expected sequence:
	//   init, codex_turn_started, codex_item_started,
	//   codex_item_completed, tool_use, tool_result,
	//   codex_turn_completed, result
	want := []string{
		"init",
		"codex_turn_started",
		"codex_item_started",
		"codex_item_completed",
		"tool_use",
		"tool_result",
		"codex_turn_completed",
		"result",
	}
	got := readSSEEvents(t, sub, len(want), 2*time.Second)
	if len(got) < len(want) {
		t.Fatalf("got %d events: %v", len(got), eventNames(got))
	}
	for i, w := range want {
		if got[i].Event != w {
			t.Errorf("event[%d] = %q, want %q (full: %v)", i, got[i].Event, w, eventNames(got))
		}
	}

	// Validate tool_use payload: the UI's handleToolUse iterates
	// data.message.content for type=="tool_use" blocks with id/name/input.
	var toolUseRaw map[string]any
	if err := json.Unmarshal(got[4].Data, &toolUseRaw); err != nil {
		t.Fatalf("decode tool_use: %v", err)
	}
	tuMsg, _ := toolUseRaw["message"].(map[string]any)
	tuContent, _ := tuMsg["content"].([]any)
	if len(tuContent) != 1 {
		t.Fatalf("tool_use content len = %d, want 1", len(tuContent))
	}
	tuBlock, _ := tuContent[0].(map[string]any)
	if tuBlock["type"] != "tool_use" {
		t.Errorf("tool_use block type = %v, want tool_use", tuBlock["type"])
	}
	if tuBlock["name"] != "Bash" {
		t.Errorf("tool_use name = %v, want Bash", tuBlock["name"])
	}
	if tuBlock["id"] != "tool-1" {
		t.Errorf("tool_use id = %v, want tool-1", tuBlock["id"])
	}
	tuInput, _ := tuBlock["input"].(map[string]any)
	if tuInput["command"] != "ls" {
		t.Errorf("tool_use input.command = %v, want ls", tuInput["command"])
	}

	// Validate tool_result payload: handleToolResult expects the user
	// message content to wrap a tool_result block keyed by tool_use_id.
	var toolResultRaw map[string]any
	if err := json.Unmarshal(got[5].Data, &toolResultRaw); err != nil {
		t.Fatalf("decode tool_result: %v", err)
	}
	if toolResultRaw["type"] != "user" {
		t.Errorf("tool_result wrapper type = %v, want user", toolResultRaw["type"])
	}
	trMsg, _ := toolResultRaw["message"].(map[string]any)
	trContent, _ := trMsg["content"].([]any)
	if len(trContent) != 1 {
		t.Fatalf("tool_result content len = %d, want 1", len(trContent))
	}
	trBlock, _ := trContent[0].(map[string]any)
	if trBlock["type"] != "tool_result" {
		t.Errorf("tool_result block type = %v, want tool_result", trBlock["type"])
	}
	if trBlock["tool_use_id"] != "tool-1" {
		t.Errorf("tool_result tool_use_id = %v, want tool-1", trBlock["tool_use_id"])
	}
	if !strings.Contains(trBlock["content"].(string), "file-a") {
		t.Errorf("tool_result content missing expected text: %v", trBlock["content"])
	}
	// IsError=false should be omitted (avoid noisy field on success).
	if _, ok := trBlock["is_error"]; ok {
		t.Errorf("tool_result is_error present on success path: %v", trBlock["is_error"])
	}
}

// TestAttachCodexBridge_AgentMessageNoDeltaEmitsCCAssistantOnComplete
// guards the path where codex skips streaming text deltas (some models
// or short turns) — we must still emit one CC-shaped assistant event on
// item_completed so the UI shows the message.
func TestAttachCodexBridge_AgentMessageNoDeltaEmitsCCAssistantOnComplete(t *testing.T) {
	sess, cb, sub := newCodexBridgeTestSession(t)
	attachCodexBridge(cb, sess, "server-create", false)

	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnStarted,
		TurnID:  "t-nd",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "running"}),
	})
	cb.emit(UnifiedEvent{
		Kind:     UEvtItemStarted,
		TurnID:   "t-nd",
		ItemID:   "msg-1",
		ItemKind: UItemAgentMessage,
		Payload:  mustMarshalRaw(UnifiedItemPayload{}),
	})
	// No UEvtItemDelta — go straight to completion with the full text.
	cb.emit(UnifiedEvent{
		Kind:     UEvtItemCompleted,
		TurnID:   "t-nd",
		ItemID:   "msg-1",
		ItemKind: UItemAgentMessage,
		Payload: mustMarshalRaw(UnifiedItemPayload{
			Result: json.RawMessage(`{"text":"final-only message"}`),
		}),
	})
	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnCompleted,
		TurnID:  "t-nd",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "ok"}),
	})

	want := []string{
		"init",
		"codex_turn_started",
		"codex_item_started",
		"codex_item_completed",
		"assistant",
		"codex_turn_completed",
		"result",
	}
	got := readSSEEvents(t, sub, len(want), 2*time.Second)
	if len(got) < len(want) {
		t.Fatalf("got %d events: %v", len(got), eventNames(got))
	}
	for i, w := range want {
		if got[i].Event != w {
			t.Errorf("event[%d] = %q, want %q (full: %v)", i, got[i].Event, w, eventNames(got))
		}
	}

	// The assistant event should carry the final text once (no duplicate
	// because no delta preceded it).
	var asstRaw map[string]any
	_ = json.Unmarshal(got[4].Data, &asstRaw)
	asstMsg, _ := asstRaw["message"].(map[string]any)
	asstContent, _ := asstMsg["content"].([]any)
	if len(asstContent) != 1 {
		t.Fatalf("assistant content len = %d, want 1", len(asstContent))
	}
	block, _ := asstContent[0].(map[string]any)
	if block["text"] != "final-only message" {
		t.Errorf("assistant text = %v, want 'final-only message'", block["text"])
	}
}

// TestAttachCodexBridge_DeltaThenCompleteSkipsDuplicateAssistant guards
// against the typewriter double-paint regression: when a delta already
// emitted a CC-shaped assistant event, the matching item_completed must
// NOT re-emit (otherwise the UI's typeBuf would receive the message twice).
func TestAttachCodexBridge_DeltaThenCompleteSkipsDuplicateAssistant(t *testing.T) {
	sess, cb, sub := newCodexBridgeTestSession(t)
	attachCodexBridge(cb, sess, "server-create", false)

	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnStarted,
		TurnID:  "t-dd",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "running"}),
	})
	cb.emit(UnifiedEvent{
		Kind:     UEvtItemStarted,
		TurnID:   "t-dd",
		ItemID:   "msg-2",
		ItemKind: UItemAgentMessage,
		Payload:  mustMarshalRaw(UnifiedItemPayload{}),
	})
	cb.emit(UnifiedEvent{
		Kind:      UEvtItemDelta,
		TurnID:    "t-dd",
		ItemID:    "msg-2",
		ItemKind:  UItemAgentMessage,
		DeltaType: "text",
		Payload:   mustMarshalRaw(UnifiedDeltaPayload{Text: "Hello"}),
	})
	cb.emit(UnifiedEvent{
		Kind:     UEvtItemCompleted,
		TurnID:   "t-dd",
		ItemID:   "msg-2",
		ItemKind: UItemAgentMessage,
		Payload: mustMarshalRaw(UnifiedItemPayload{
			Result: json.RawMessage(`{"text":"Hello"}`),
		}),
	})
	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnCompleted,
		TurnID:  "t-dd",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "ok"}),
	})

	got := readSSEEvents(t, sub, 12, 2*time.Second)
	// Count CC-shaped "assistant" events — must be exactly 1 (from the delta).
	count := 0
	for _, ev := range got {
		if ev.Event == "assistant" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("CC-shaped assistant events = %d, want 1 (sequence: %v)", count, eventNames(got))
	}
}

// TestAwaitBackendReadyDistinguishesDeathFromSlowInit covers the failure mode
// that used to be swallowed. waitInit returns false both when a backend is
// merely slow and when it has already died, and every "inject the first
// message" call site treated the two identically: the message went to a dead
// process, the write failed, the goroutine logged and returned, and the
// session stayed non-terminal until the CLI reported it as a completed run.
// A codex handshake that failed on a removed protocol field therefore looked
// exactly like a successful dispatch that did no work.
func TestAwaitBackendReadyDistinguishesDeathFromSlowInit(t *testing.T) {
	newSession := func(t *testing.T, id string) (*serverSession, *codexBackend) {
		t.Helper()
		// Sandbox appDir so any JSONL write stays out of the real data dir.
		origAppDir := appDir
		appDir = t.TempDir()
		t.Cleanup(func() { appDir = origAppDir })

		sess := &serverSession{ID: id, broadcaster: newBroadcaster(), Backend: BackendCodex}
		cb := newCodexBackend(SessionOpts{Model: "gpt-test"})
		sess.process = cb
		return sess, cb
	}

	t.Run("dead backend is not ready and fails the session", func(t *testing.T) {
		sess, cb := newSession(t, "dead-session")
		cb.markDone() // process exited mid-handshake

		if sess.awaitBackendReady(200 * time.Millisecond) {
			t.Fatal("awaitBackendReady reported ready for a dead backend")
		}
		sess.mu.Lock()
		got := sess.Status
		sess.mu.Unlock()
		if got != "error" {
			t.Fatalf("dead backend must leave the session terminal; want status %q, got %q", "error", got)
		}
	})

	t.Run("slow but alive backend is still ready", func(t *testing.T) {
		sess, cb := newSession(t, "slow-session")
		// Never signals init and never exits: waitInit times out while
		// alive() stays true. Dropping the first message here would be wrong.
		t.Cleanup(cb.markDone)

		if !sess.awaitBackendReady(100 * time.Millisecond) {
			t.Fatal("awaitBackendReady reported not-ready for a live backend that was merely slow")
		}
		sess.mu.Lock()
		got := sess.Status
		sess.mu.Unlock()
		if got == "error" {
			t.Fatalf("a slow-but-alive backend must not fail the session, got status %q", got)
		}
	})
}

func eventNames(evs []sseEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Event
	}
	return out
}
