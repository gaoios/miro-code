package app

import (
	"encoding/json"
	"fmt"
	"os"
)

func attachKimiBridge(kb *kimiBackend, sess *serverSession, source string, fullSync bool) {
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		state := false
		for {
			select {
			case ev := <-kb.events():
				handleKimiUnifiedEvent(kb, sess, ev, &state, source, fullSync)
			case <-kb.done:
				drainKimiEvents(kb, sess, &state, source, fullSync)
				if !kb.suppressNextCloseFlag.Load() {
					sess.broadcaster.broadcast(sseEvent{Event: "close", Data: []byte(`{"reason":"process_exited","backend":"kimi"}`)})
				}
				sess.mu.Lock()
				if sess.bridgeDone == doneCh {
					sess.bridgeDone = nil
				}
				sess.mu.Unlock()
				return
			}
		}
	}()
	sess.mu.Lock()
	sess.bridgeDone = doneCh
	sess.mu.Unlock()
	go watchKimiExit(kb, sess)
}

func drainKimiEvents(kb *kimiBackend, sess *serverSession, initialized *bool, source string, fullSync bool) {
	for i := 0; i < 1000; i++ {
		select {
		case ev := <-kb.events():
			handleKimiUnifiedEvent(kb, sess, ev, initialized, source, fullSync)
		default:
			return
		}
	}
	fmt.Fprintf(os.Stderr, "[%s] kimi: event drain reached safety cap\n", appName)
}

func handleKimiUnifiedEvent(kb *kimiBackend, sess *serverSession, ev UnifiedEvent, initialized *bool, source string, fullSync bool) {
	if !*initialized && (ev.Kind == UEvtTurnStarted || ev.Kind == UEvtItemStarted) {
		*initialized = true
		emitKimiSyntheticInit(kb, sess, source, fullSync)
	}
	switch ev.Kind {
	case UEvtTurnStarted:
		sess.broadcaster.broadcast(sseEvent{Event: "kimi_turn_started", Data: kimiEventEnvelope(ev)})
		sess.touch()
	case UEvtTurnCompleted:
		var payload UnifiedTurnPayload
		_ = json.Unmarshal(ev.Payload, &payload)
		status := "idle"
		if payload.Status == "error" {
			status = "error"
		}
		sess.mu.Lock()
		sess.NumTurns++
		sess.mu.Unlock()
		sess.setStatus(status)
		if fullSync {
			go func() { _, _ = incrementUserTurns(sess.ID) }()
		}
		sess.broadcaster.broadcast(sseEvent{Event: "kimi_turn_completed", Data: kimiEventEnvelope(ev)})
		sess.broadcaster.broadcast(sseEvent{Event: "result", Data: kimiResultEnvelope(ev)})
	case UEvtItemStarted:
		sess.broadcaster.broadcast(sseEvent{Event: "kimi_item_started", Data: kimiEventEnvelope(ev)})
	case UEvtItemDelta:
		sess.broadcaster.broadcast(sseEvent{Event: "kimi_item_delta", Data: kimiEventEnvelope(ev)})
	case UEvtItemCompleted:
		sess.broadcaster.broadcast(sseEvent{Event: "kimi_item_completed", Data: kimiEventEnvelope(ev)})
		persistKimiItemCompleted(kb, ev)
		broadcastKimiItemCompletedAsCC(kb, sess, ev)
	case UEvtBackendError:
		sess.broadcaster.broadcast(sseEvent{Event: "kimi_backend_error", Data: kimiEventEnvelope(ev)})
	}
}

func emitKimiSyntheticInit(kb *kimiBackend, sess *serverSession, source string, fullSync bool) {
	info := kb.info()
	msg := map[string]any{
		"type": "system", "subtype": "init", "cwd": kb.cwd,
		"session_id": info.SessionID, "model": info.Model,
		"tools": []string{}, "mcp_servers": []map[string]any{},
		"permissionMode": "bypassPermissions",
		"backend":        map[string]any{"kind": string(info.Kind), "model": info.Model},
	}
	raw, _ := json.Marshal(msg)
	sess.broadcaster.broadcast(sseEvent{Event: "init", Data: raw})
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
	if info.SessionID != "" {
		writeCodexSystemInit(info.SessionID, info.Model, kb.cwd)
	}
}

func kimiEventEnvelope(ev UnifiedEvent) []byte {
	raw, _ := json.Marshal(ev)
	return raw
}

func kimiResultEnvelope(ev UnifiedEvent) []byte {
	var payload UnifiedTurnPayload
	_ = json.Unmarshal(ev.Payload, &payload)
	subtype := "success"
	isErr := false
	if payload.Status == "error" {
		subtype, isErr = "error", true
	} else if payload.Status == "cancelled" {
		subtype = "cancelled"
	}
	raw, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": subtype, "is_error": isErr,
		"result": payload.Error, "session_id": ev.TurnID,
		"total_cost_usd": payload.CostUSD, "backend": "kimi",
	})
	return raw
}

func persistKimiItemCompleted(kb *kimiBackend, ev UnifiedEvent) {
	info := kb.info()
	if info.SessionID == "" || ev.ItemKind != UItemAgentMessage {
		return
	}
	var payload UnifiedItemPayload
	_ = json.Unmarshal(ev.Payload, &payload)
	if text := extractCodexResultText(payload.Result); text != "" {
		writeCodexAssistantMessage(info.SessionID, info.Model, text)
	}
}

func broadcastKimiItemCompletedAsCC(kb *kimiBackend, sess *serverSession, ev UnifiedEvent) {
	if ev.ItemKind != UItemAgentMessage {
		return
	}
	var payload UnifiedItemPayload
	_ = json.Unmarshal(ev.Payload, &payload)
	text := extractCodexResultText(payload.Result)
	if text == "" {
		return
	}
	raw, _ := json.Marshal(map[string]any{
		"type": "assistant", "timestamp": nowRFC3339(),
		"message": map[string]any{"role": "assistant", "model": kb.info().Model, "content": []map[string]any{{"type": "text", "text": text}}},
	})
	sess.broadcaster.broadcast(sseEvent{Event: "assistant", Data: raw})
}

func watchKimiExit(kb *kimiBackend, sess *serverSession) {
	<-kb.done
	sess.mu.Lock()
	alreadyStopped := sess.Status == "stopped"
	sess.mu.Unlock()
	if alreadyStopped {
		return
	}
	if kb.exitCode.Load() != 0 || kb.rateLimited.Load() {
		sess.setStatus("error")
		return
	}
	sess.setStatus("stopped")
}
