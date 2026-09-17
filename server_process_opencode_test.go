package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAttachOpencodeBridgeTurnFlowEmitsSSE(t *testing.T) {
	origAppDir := appDir
	appDir = t.TempDir()
	t.Cleanup(func() { appDir = origAppDir })

	bc := newBroadcaster()
	sess := &serverSession{ID: "test-session", Project: "/tmp", broadcaster: bc, Backend: BackendOpencode, tasks: newTaskTracker()}
	kb := &opencodeBackend{cwd: "/tmp", model: "opencode-test", sessionID: "opencode-session", eventsCh: make(chan UnifiedEvent, opencodeEventsChanCapacity), done: make(chan struct{})}
	sub := bc.subscribe()
	t.Cleanup(func() {
		bc.unsubscribe(sub)
		kb.shutdown()
		sess.waitBridgeDone()
	})
	attachOpencodeBridge(kb, sess, "server-create", false)

	kb.emit(UnifiedEvent{Kind: UEvtTurnStarted, TurnID: "turn-1", Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "running"})})
	kb.emit(UnifiedEvent{Kind: UEvtItemStarted, TurnID: "turn-1", ItemID: "item-1", ItemKind: UItemAgentMessage, Payload: mustMarshalRaw(UnifiedItemPayload{})})
	kb.emit(UnifiedEvent{Kind: UEvtItemCompleted, TurnID: "turn-1", ItemID: "item-1", ItemKind: UItemAgentMessage, Payload: mustMarshalRaw(UnifiedItemPayload{Result: mustMarshalRaw(map[string]any{"text": "hello from opencode"})})})
	kb.emit(UnifiedEvent{Kind: UEvtTurnCompleted, TurnID: "turn-1", Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "ok"})})

	want := []string{"init", "opencode_turn_started", "opencode_item_started", "opencode_item_completed", "assistant", "opencode_turn_completed", "result"}
	got := readSSEEvents(t, sub, len(want), 2*time.Second)
	for i, name := range want {
		if i >= len(got) || got[i].Event != name {
			t.Fatalf("event sequence = %v, want %v", eventNames(got), want)
		}
	}
	var initPayload map[string]any
	if err := json.Unmarshal(got[0].Data, &initPayload); err != nil {
		t.Fatal(err)
	}
	backend, _ := initPayload["backend"].(map[string]any)
	if backend["kind"] != "opencode" {
		t.Fatalf("init backend.kind = %v, want opencode", backend["kind"])
	}
	var resultPayload map[string]any
	_ = json.Unmarshal(got[6].Data, &resultPayload)
	if resultPayload["backend"] != "opencode" || resultPayload["subtype"] != "success" {
		t.Fatalf("bad result payload: %v", resultPayload)
	}
	sess.mu.Lock()
	status, turns := sess.Status, sess.NumTurns
	sess.mu.Unlock()
	if status != "idle" || turns != 1 {
		t.Fatalf("session status=%q turns=%d, want idle and 1", status, turns)
	}
}
