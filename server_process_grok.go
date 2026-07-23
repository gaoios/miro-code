package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// attachGrokBridge consumes a *grokBackend's unified-event stream and mirrors
// it to the session SSE broadcaster. Grok's backend currently emits only final
// assistant text per turn, but the bridge keeps the same typed-event shape as
// codex so future Grok streaming/tool events can slot in without changing the
// session layer again.
func attachGrokBridge(gb *grokBackend, sess *serverSession, source string, fullSync bool) {
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		runGrokEventLoop(gb, sess, source, fullSync)
		sess.dismissAllPendingAUQ("session_dead")
		if cancelled := sess.tasks.markAllRunningAsCancelled(); len(cancelled) > 0 {
			for _, t := range cancelled {
				if data, err := json.Marshal(t); err == nil {
					sess.broadcaster.broadcast(sseEvent{Event: "task_event", Data: data})
				}
			}
		}
		if !gb.suppressNextCloseFlag.Load() {
			sess.broadcaster.broadcast(sseEvent{
				Event: "close",
				Data:  []byte(`{"reason":"process_exited","backend":"grok"}`),
			})
		}
		sess.mu.Lock()
		if sess.bridgeDone == doneCh {
			sess.bridgeDone = nil
		}
		sess.mu.Unlock()
	}()
	sess.mu.Lock()
	sess.bridgeDone = doneCh
	sess.mu.Unlock()
	go watchGrokExit(gb, sess)
}

type grokBridgeState struct {
	initEmitted bool
}

func runGrokEventLoop(gb *grokBackend, sess *serverSession, source string, fullSync bool) {
	state := &grokBridgeState{}
	for {
		select {
		case ev := <-gb.events():
			handleGrokUnifiedEvent(gb, sess, ev, state, source, fullSync)
		case <-gb.done:
			drainGrokEvents(gb, sess, state, source, fullSync)
			return
		}
	}
}

func drainGrokEvents(gb *grokBackend, sess *serverSession, state *grokBridgeState, source string, fullSync bool) {
	const maxDrainIters = 1000
	for i := 0; i < maxDrainIters; i++ {
		select {
		case ev := <-gb.events():
			handleGrokUnifiedEvent(gb, sess, ev, state, source, fullSync)
		default:
			return
		}
	}
	fmt.Fprintf(os.Stderr, "[%s] grok: drainGrokEvents hit max iter cap (%d)\n", appName, maxDrainIters)
}

func handleGrokUnifiedEvent(gb *grokBackend, sess *serverSession, ev UnifiedEvent, state *grokBridgeState, source string, fullSync bool) {
	if !state.initEmitted && (ev.Kind == UEvtTurnStarted || ev.Kind == UEvtItemStarted) {
		state.initEmitted = true
		emitGrokSyntheticInit(gb, sess, source, fullSync)
	}

	switch ev.Kind {
	case UEvtTurnStarted:
		sess.broadcaster.broadcast(sseEvent{Event: "grok_turn_started", Data: grokEventEnvelope(ev)})
		sess.touch()

	case UEvtTurnCompleted:
		var payload UnifiedTurnPayload
		_ = json.Unmarshal(ev.Payload, &payload)
		newStatus := "idle"
		if payload.Status == "error" {
			newStatus = "error"
		}
		sess.mu.Lock()
		sess.NumTurns++
		if payload.CostUSD > 0 {
			sess.TotalCost += payload.CostUSD
		}
		sess.mu.Unlock()
		sess.setStatus(newStatus)
		if fullSync {
			go func() { _, _ = incrementUserTurns(sess.ID) }()
		}
		sess.broadcaster.broadcast(sseEvent{Event: "grok_turn_completed", Data: grokEventEnvelope(ev)})
		sess.broadcaster.broadcast(sseEvent{Event: "result", Data: grokResultEnvelope(ev)})

	case UEvtItemStarted:
		sess.broadcaster.broadcast(sseEvent{Event: "grok_item_started", Data: grokEventEnvelope(ev)})

	case UEvtItemDelta:
		sess.broadcaster.broadcast(sseEvent{Event: "grok_item_delta", Data: grokEventEnvelope(ev)})

	case UEvtItemCompleted:
		sess.broadcaster.broadcast(sseEvent{Event: "grok_item_completed", Data: grokEventEnvelope(ev)})
		persistGrokItemCompleted(gb, sess, ev)
		broadcastGrokItemCompletedAsCC(gb, sess, ev)

	case UEvtBackendError:
		sess.broadcaster.broadcast(sseEvent{Event: "grok_backend_error", Data: grokEventEnvelope(ev)})
	}
}

func emitGrokSyntheticInit(gb *grokBackend, sess *serverSession, source string, fullSync bool) {
	info := gb.info()
	msg := map[string]any{
		"type":           "system",
		"subtype":        "init",
		"cwd":            gb.cwd,
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
		writeCodexSystemInit(info.SessionID, info.Model, gb.cwd)
	}
}

func grokEventEnvelope(ev UnifiedEvent) []byte {
	raw, _ := json.Marshal(ev)
	return raw
}

func grokResultEnvelope(ev UnifiedEvent) []byte {
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
		"backend":        "grok",
	}
	raw, _ := json.Marshal(out)
	return raw
}

func persistGrokItemCompleted(gb *grokBackend, sess *serverSession, ev UnifiedEvent) {
	info := gb.info()
	if info.SessionID == "" {
		return
	}
	var payload UnifiedItemPayload
	_ = json.Unmarshal(ev.Payload, &payload)
	resultText := extractCodexResultText(payload.Result)
	if ev.ItemKind == UItemAgentMessage && resultText != "" {
		writeCodexAssistantMessage(info.SessionID, info.Model, resultText)
	}
}

func broadcastGrokItemCompletedAsCC(gb *grokBackend, sess *serverSession, ev UnifiedEvent) {
	if ev.ItemKind != UItemAgentMessage {
		return
	}
	var payload UnifiedItemPayload
	_ = json.Unmarshal(ev.Payload, &payload)
	resultText := extractCodexResultText(payload.Result)
	if resultText == "" {
		return
	}
	msg := map[string]any{
		"type":      "assistant",
		"timestamp": nowRFC3339(),
		"message": map[string]any{
			"role":  "assistant",
			"model": gb.info().Model,
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
}

func watchGrokExit(gb *grokBackend, sess *serverSession) {
	<-gb.done
	sess.mu.Lock()
	alreadyStopped := sess.Status == "stopped"
	sess.mu.Unlock()
	if alreadyStopped {
		return
	}
	if errPtr := gb.initErr.Load(); errPtr != nil && *errPtr != "" {
		fmt.Fprintf(os.Stderr, "[%s] server: grok session %s init error: %s\n",
			appName, shortID(sess.ID), *errPtr)
		sess.setStatus("error")
		return
	}
	if exitCode := gb.exitCode.Load(); exitCode != 0 || gb.rateLimited.Load() {
		fmt.Fprintf(os.Stderr, "[%s] server: grok session %s exited code=%d rate_limited=%v\n",
			appName, shortID(sess.ID), exitCode, gb.rateLimited.Load())
		sess.setStatus("error")
		return
	}
	sess.setStatus("stopped")
}
