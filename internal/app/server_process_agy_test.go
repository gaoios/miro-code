package app

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAttachAgyBridgeTurnFlowEmitsSSE(t *testing.T) {
	origAppDir := appDir
	appDir = t.TempDir()
	t.Cleanup(func() { appDir = origAppDir })

	bc := newBroadcaster()
	sess := &serverSession{ID: "test-session", Project: "/tmp", broadcaster: bc, Backend: BackendAgy, tasks: newTaskTracker()}
	kb := &agyBackend{cwd: "/tmp", model: "agy-test", sessionID: "agy-session", eventsCh: make(chan UnifiedEvent, agyEventsChanCapacity), done: make(chan struct{})}
	sub := bc.subscribe()
	t.Cleanup(func() {
		bc.unsubscribe(sub)
		kb.shutdown()
		sess.waitBridgeDone()
	})
	attachAgyBridge(kb, sess, "server-create", false)

	kb.emit(UnifiedEvent{Kind: UEvtTurnStarted, TurnID: "turn-1", Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "running"})})
	kb.emit(UnifiedEvent{Kind: UEvtItemStarted, TurnID: "turn-1", ItemID: "item-1", ItemKind: UItemAgentMessage, Payload: mustMarshalRaw(UnifiedItemPayload{})})
	kb.emit(UnifiedEvent{Kind: UEvtItemCompleted, TurnID: "turn-1", ItemID: "item-1", ItemKind: UItemAgentMessage, Payload: mustMarshalRaw(UnifiedItemPayload{Result: mustMarshalRaw(map[string]any{"text": "hello from agy"})})})
	kb.emit(UnifiedEvent{Kind: UEvtTurnCompleted, TurnID: "turn-1", Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "ok"})})

	want := []string{"init", "agy_turn_started", "agy_item_started", "agy_item_completed", "assistant", "agy_turn_completed", "result"}
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
	if backend["kind"] != "agy" {
		t.Fatalf("init backend.kind = %v, want agy", backend["kind"])
	}
	var resultPayload map[string]any
	_ = json.Unmarshal(got[6].Data, &resultPayload)
	if resultPayload["backend"] != "agy" || resultPayload["subtype"] != "success" {
		t.Fatalf("bad result payload: %v", resultPayload)
	}
}
