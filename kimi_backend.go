package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const kimiEventsChanCapacity = 64

// kimiBackend runs Kimi CLI in bounded print mode, spawning one process per
// user turn while preserving a stable Soul server session identity.
type kimiBackend struct {
	bin       string
	cwd       string
	model     string
	sessionID string
	eventsCh  chan UnifiedEvent
	done      chan struct{}

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	closed  bool

	suppressNextCloseFlag atomic.Bool
	rateLimited           atomic.Bool
	exitCode              atomic.Int32
	initErr               atomic.Pointer[string]
	turnSeq               atomic.Int64
}

func spawnKimi(opts SessionOpts) (*kimiBackend, error) {
	bin := kimiBinary
	if bin == "" {
		bin = "kimi"
	}
	resolvedBin, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("kimi binary %q not found: %w", bin, err)
	}
	cwd := opts.WorkDir
	if cwd == "" {
		cwd = workspace
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	sessionID := opts.ResumeID
	if sessionID == "" {
		sessionID = opts.ServerSessionID
	}
	if sessionID == "" {
		sessionID = fmt.Sprintf("kimi-%d", time.Now().UnixNano())
	}
	return &kimiBackend{
		bin:       resolvedBin,
		cwd:       cwd,
		model:     kimiResolveModel(opts.Model),
		sessionID: sessionID,
		eventsCh:  make(chan UnifiedEvent, kimiEventsChanCapacity),
		done:      make(chan struct{}),
	}, nil
}

func (kb *kimiBackend) info() BackendInfo {
	if kb == nil {
		return BackendInfo{Kind: BackendKimi}
	}
	return BackendInfo{Kind: BackendKimi, Model: kb.model, SessionID: kb.sessionID}
}

func (kb *kimiBackend) alive() bool {
	if kb == nil {
		return false
	}
	kb.mu.Lock()
	defer kb.mu.Unlock()
	return !kb.closed
}

func (kb *kimiBackend) waitInit(time.Duration) bool {
	return kb != nil && kb.initErr.Load() == nil
}

func (kb *kimiBackend) sendMessage(content string) error {
	if kb == nil {
		return fmt.Errorf("kimi backend is nil")
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("empty message")
	}
	kb.mu.Lock()
	if kb.closed {
		kb.mu.Unlock()
		return fmt.Errorf("kimi backend is shut down")
	}
	if kb.running {
		kb.mu.Unlock()
		return fmt.Errorf("kimi turn already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	kb.running = true
	kb.cancel = cancel
	turnID := fmt.Sprintf("%s-turn-%d", kb.sessionID, kb.turnSeq.Add(1))
	kb.mu.Unlock()
	go kb.runTurn(ctx, turnID, content)
	return nil
}

func (kb *kimiBackend) runTurn(ctx context.Context, turnID, content string) {
	defer func() {
		kb.mu.Lock()
		kb.running = false
		kb.cancel = nil
		kb.mu.Unlock()
	}()
	kb.emit(UnifiedEvent{Kind: UEvtTurnStarted, TurnID: turnID, Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "running"})})

	cmd := exec.CommandContext(ctx, kb.bin, kb.commandArgs(content)...)
	cmd.Dir = kb.cwd
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		kb.exitCode.Store(int32(commandExitCode(err)))
		if looksLikeRateLimit(strings.ToLower(message)) {
			kb.markRateLimited()
		}
		kb.emitError(turnID, message)
		return
	}
	text, err := parseKimiTextOutput(stdout)
	if err != nil {
		kb.emitError(turnID, err.Error())
		return
	}
	itemID := turnID + "-item-1"
	kb.emit(UnifiedEvent{Kind: UEvtItemStarted, TurnID: turnID, ItemID: itemID, ItemKind: UItemAgentMessage, Payload: mustMarshalRaw(UnifiedItemPayload{})})
	kb.emit(UnifiedEvent{Kind: UEvtItemCompleted, TurnID: turnID, ItemID: itemID, ItemKind: UItemAgentMessage, Payload: mustMarshalRaw(UnifiedItemPayload{Result: mustMarshalRaw(map[string]any{"text": text})})})
	kb.emit(UnifiedEvent{Kind: UEvtTurnCompleted, TurnID: turnID, Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "ok"})})
}

func parseKimiTextOutput(raw []byte) (string, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "", fmt.Errorf("empty kimi output")
	}
	if strings.EqualFold(text, "LLM not set") {
		return "", fmt.Errorf("kimi LLM not configured; run `kimi login` or configure ~/.kimi/config.toml")
	}
	return text, nil
}

func (kb *kimiBackend) emitError(turnID, message string) {
	kb.emit(UnifiedEvent{Kind: UEvtBackendError, TurnID: turnID, Payload: mustMarshalRaw(map[string]any{"error": message})})
	kb.emit(UnifiedEvent{Kind: UEvtTurnCompleted, TurnID: turnID, Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "error", Error: message})})
}

func (kb *kimiBackend) commandArgs(content string) []string {
	args := []string{"--print", "--output-format", "text", "--final-message-only", "--work-dir", kb.cwd, "--prompt", content}
	if kb.model != "" {
		args = append(args, "--model", kb.model)
	}
	return args
}

func (kb *kimiBackend) events() <-chan UnifiedEvent {
	if kb == nil {
		ch := make(chan UnifiedEvent)
		close(ch)
		return ch
	}
	return kb.eventsCh
}

func (kb *kimiBackend) emit(ev UnifiedEvent) {
	if kb == nil {
		return
	}
	select {
	case kb.eventsCh <- ev:
	case <-kb.done:
	}
}

func (kb *kimiBackend) sendPermissionDecision(string, map[string]any) error {
	return fmt.Errorf("kimi backend does not support permission decisions")
}

func (kb *kimiBackend) controlRequest(subtype string, extra map[string]any) error {
	switch subtype {
	case "interrupt":
		kb.mu.Lock()
		cancel := kb.cancel
		kb.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	case "set_model":
		model, _ := extra["model"].(string)
		if model == "" {
			return fmt.Errorf("set_model requires model")
		}
		kb.mu.Lock()
		kb.model = kimiResolveModel(model)
		kb.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("kimi backend control request %q not supported", subtype)
	}
}

func (kb *kimiBackend) controlRequestSync(subtype string, _ map[string]any, _ time.Duration) (json.RawMessage, error) {
	return nil, fmt.Errorf("kimi backend control request %q not supported", subtype)
}

func (kb *kimiBackend) shutdown() {
	if kb == nil {
		return
	}
	kb.mu.Lock()
	if kb.closed {
		kb.mu.Unlock()
		return
	}
	kb.closed = true
	cancel := kb.cancel
	close(kb.done)
	kb.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (kb *kimiBackend) suppressNextClose() {
	if kb != nil {
		kb.suppressNextCloseFlag.Store(true)
	}
}

func (kb *kimiBackend) markRateLimited() {
	if kb != nil {
		kb.rateLimited.Store(true)
	}
}

func (kb *kimiBackend) killProcess() {
	if kb != nil {
		_ = kb.controlRequest("interrupt", nil)
	}
}
