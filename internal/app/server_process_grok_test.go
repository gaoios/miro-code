package app

import (
	"encoding/json"
	"testing"
	"time"
)

func newGrokBridgeTestSession(t *testing.T) (*serverSession, *grokBackend, *subscriber) {
	t.Helper()
	origAppDir := appDir
	appDir = t.TempDir()
	t.Cleanup(func() { appDir = origAppDir })

	bc := newBroadcaster()
	sess := &serverSession{
		ID:          "test-session",
		Project:     "/tmp",
		broadcaster: bc,
		Backend:     BackendGrok,
		tasks:       newTaskTracker(),
	}
	gb := &grokBackend{
		cwd:       "/tmp",
		model:     "grok-test",
		sessionID: "grok-session",
		eventsCh:  make(chan UnifiedEvent, grokEventsChanCapacity),
		done:      make(chan struct{}),
	}
	sub := bc.subscribe()
	t.Cleanup(func() {
		bc.unsubscribe(sub)
		gb.shutdown()
		sess.waitBridgeDone()
	})
	return sess, gb, sub
}

func TestAttachGrokBridgeTurnFlowEmitsSSE(t *testing.T) {
	sess, gb, sub := newGrokBridgeTestSession(t)
	attachGrokBridge(gb, sess, "server-create", false)

	gb.emit(UnifiedEvent{
		Kind:    UEvtTurnStarted,
		TurnID:  "turn-1",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "running"}),
	})
	gb.emit(UnifiedEvent{
		Kind:     UEvtItemStarted,
		TurnID:   "turn-1",
		ItemID:   "item-1",
		ItemKind: UItemAgentMessage,
		Payload:  mustMarshalRaw(UnifiedItemPayload{}),
	})
	gb.emit(UnifiedEvent{
		Kind:     UEvtItemCompleted,
		TurnID:   "turn-1",
		ItemID:   "item-1",
		ItemKind: UItemAgentMessage,
		Payload: mustMarshalRaw(UnifiedItemPayload{
			Result: mustMarshalRaw(map[string]any{"text": "hello from grok"}),
		}),
	})
	gb.emit(UnifiedEvent{
		Kind:    UEvtTurnCompleted,
		TurnID:  "turn-1",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "ok"}),
	})

	want := []string{
		"init",
		"grok_turn_started",
		"grok_item_started",
		"grok_item_completed",
		"assistant",
		"grok_turn_completed",
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

	var initPayload map[string]any
	if err := json.Unmarshal(got[0].Data, &initPayload); err != nil {
		t.Fatalf("decode init payload: %v", err)
	}
	backendField, _ := initPayload["backend"].(map[string]any)
	if backendField["kind"] != string(BackendGrok) {
		t.Errorf("init backend.kind = %v, want grok", backendField["kind"])
	}

	var assistantPayload map[string]any
	if err := json.Unmarshal(got[4].Data, &assistantPayload); err != nil {
		t.Fatalf("decode assistant payload: %v", err)
	}
	msg, _ := assistantPayload["message"].(map[string]any)
	contents, _ := msg["content"].([]any)
	if len(contents) != 1 {
		t.Fatalf("assistant content len = %d, want 1", len(contents))
	}
	first, _ := contents[0].(map[string]any)
	if first["text"] != "hello from grok" {
		t.Errorf("assistant text = %v, want hello from grok", first["text"])
	}

	var resultPayload map[string]any
	if err := json.Unmarshal(got[6].Data, &resultPayload); err != nil {
		t.Fatalf("decode result payload: %v", err)
	}
	if resultPayload["backend"] != "grok" || resultPayload["subtype"] != "success" {
		t.Errorf("bad result payload: %v", resultPayload)
	}

	sess.mu.Lock()
	gotStatus := sess.Status
	sess.mu.Unlock()
	if gotStatus != "idle" {
		t.Errorf("session status = %q, want idle", gotStatus)
	}
}
