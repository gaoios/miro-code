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

const grokEventsChanCapacity = 64

// grokBackend runs Grok Build in headless one-shot mode. Unlike codex's
// JSON-RPC app-server or Claude Code's stream-json subprocess, Grok Build's
// stable CLI surface here is `grok -p ... --output-format json`; we keep one
// backend object per weiran session and spawn one Grok process per user turn.
type grokBackend struct {
	bin       string
	cwd       string
	model     string
	sessionID string

	eventsCh chan UnifiedEvent
	done     chan struct{}

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

type grokHeadlessOutput struct {
	Text       string `json:"text"`
	SessionID  string `json:"sessionId"`
	StopReason string `json:"stopReason"`
	RequestID  string `json:"requestId"`
}

func spawnGrok(opts SessionOpts) (*grokBackend, error) {
	bin := grokBinary
	if bin == "" {
		bin = "grok"
	}
	resolvedBin, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("grok binary %q not found: %w", bin, err)
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
		sessionID = fmt.Sprintf("grok-%d", time.Now().UnixNano())
	}
	gb := &grokBackend{
		bin:       resolvedBin,
		cwd:       cwd,
		model:     grokResolveModel(opts.Model),
		sessionID: sessionID,
		eventsCh:  make(chan UnifiedEvent, grokEventsChanCapacity),
		done:      make(chan struct{}),
	}
	return gb, nil
}

func parseGrokHeadlessOutput(raw []byte) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", fmt.Errorf("empty grok output")
	}
	var out grokHeadlessOutput
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return "", fmt.Errorf("parse grok json output: %w", err)
	}
	if strings.TrimSpace(out.Text) == "" {
		return "", fmt.Errorf("grok json output missing text")
	}
	return out.Text, nil
}

func (gb *grokBackend) info() BackendInfo {
	if gb == nil {
		return BackendInfo{Kind: BackendGrok}
	}
	return BackendInfo{Kind: BackendGrok, Model: gb.model, SessionID: gb.sessionID}
}

func (gb *grokBackend) alive() bool {
	if gb == nil {
		return false
	}
	gb.mu.Lock()
	defer gb.mu.Unlock()
	return !gb.closed
}

func (gb *grokBackend) waitInit(timeout time.Duration) bool {
	if gb == nil {
		return false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-gb.done:
		return false
	case <-timer.C:
		return !gb.initFailed()
	default:
		return !gb.initFailed()
	}
}

func (gb *grokBackend) sendMessage(content string) error {
	if gb == nil {
		return fmt.Errorf("grok backend is nil")
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("empty message")
	}
	gb.mu.Lock()
	if gb.closed {
		gb.mu.Unlock()
		return fmt.Errorf("grok backend is shut down")
	}
	if gb.running {
		gb.mu.Unlock()
		return fmt.Errorf("grok turn already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	gb.running = true
	gb.cancel = cancel
	turnSeq := gb.turnSeq.Add(1)
	gb.mu.Unlock()

	turnID := fmt.Sprintf("%s-turn-%d", gb.sessionID, turnSeq)
	go gb.runTurn(ctx, turnID, content)
	return nil
}

func (gb *grokBackend) runTurn(ctx context.Context, turnID, content string) {
	defer func() {
		gb.mu.Lock()
		gb.running = false
		gb.cancel = nil
		gb.mu.Unlock()
	}()

	gb.emit(UnifiedEvent{
		Kind:    UEvtTurnStarted,
		TurnID:  turnID,
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "running"}),
	})

	args := gb.commandArgs(content)
	cmd := exec.CommandContext(ctx, gb.bin, args...)
	cmd.Dir = gb.cwd
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		gb.exitCode.Store(int32(commandExitCode(err)))
		if looksLikeRateLimit(strings.ToLower(msg)) {
			gb.markRateLimited()
		}
		gb.emitBackendError(turnID, msg)
		gb.emit(UnifiedEvent{
			Kind:    UEvtTurnCompleted,
			TurnID:  turnID,
			Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "error", Error: msg}),
		})
		return
	}

	text, err := parseGrokHeadlessOutput(stdout)
	if err != nil {
		msg := err.Error()
		gb.emitBackendError(turnID, msg)
		gb.emit(UnifiedEvent{
			Kind:    UEvtTurnCompleted,
			TurnID:  turnID,
			Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "error", Error: msg}),
		})
		return
	}

	itemID := fmt.Sprintf("%s-item-1", turnID)
	gb.emit(UnifiedEvent{
		Kind:     UEvtItemStarted,
		TurnID:   turnID,
		ItemID:   itemID,
		ItemKind: UItemAgentMessage,
		Payload:  mustMarshalRaw(UnifiedItemPayload{}),
	})
	gb.emit(UnifiedEvent{
		Kind:     UEvtItemCompleted,
		TurnID:   turnID,
		ItemID:   itemID,
		ItemKind: UItemAgentMessage,
		Payload: mustMarshalRaw(UnifiedItemPayload{
			Result: mustMarshalRaw(map[string]any{"text": text}),
		}),
	})
	gb.emit(UnifiedEvent{
		Kind:    UEvtTurnCompleted,
		TurnID:  turnID,
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "ok"}),
	})
}

func (gb *grokBackend) commandArgs(content string) []string {
	args := []string{
		"-p", content,
		"--cwd", gb.cwd,
		"--output-format", "json",
		"--max-turns", "1",
		"--no-subagents",
		"--disable-web-search",
		"--no-memory",
	}
	if gb.model != "" {
		args = append(args, "--model", gb.model)
	}
	return args
}

func (gb *grokBackend) events() <-chan UnifiedEvent {
	if gb == nil {
		ch := make(chan UnifiedEvent)
		close(ch)
		return ch
	}
	return gb.eventsCh
}

func (gb *grokBackend) emit(ev UnifiedEvent) {
	if gb == nil {
		return
	}
	select {
	case gb.eventsCh <- ev:
	case <-gb.done:
	}
}

func (gb *grokBackend) emitBackendError(turnID, message string) {
	gb.emit(UnifiedEvent{
		Kind:    UEvtBackendError,
		TurnID:  turnID,
		Payload: mustMarshalRaw(map[string]any{"error": message}),
	})
}

func (gb *grokBackend) sendPermissionDecision(requestID string, decision map[string]any) error {
	return fmt.Errorf("grok backend does not support permission decisions")
}

func (gb *grokBackend) controlRequest(subtype string, extra map[string]any) error {
	switch subtype {
	case "interrupt":
		gb.mu.Lock()
		cancel := gb.cancel
		gb.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	case "set_model":
		model, _ := extra["model"].(string)
		if model == "" {
			return fmt.Errorf("set_model requires model")
		}
		gb.mu.Lock()
		gb.model = grokResolveModel(model)
		gb.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("grok backend control request %q not supported", subtype)
	}
}

func (gb *grokBackend) controlRequestSync(subtype string, extra map[string]any, timeout time.Duration) (json.RawMessage, error) {
	return nil, fmt.Errorf("grok backend control request %q not supported", subtype)
}

func (gb *grokBackend) shutdown() {
	if gb == nil {
		return
	}
	gb.mu.Lock()
	if gb.closed {
		gb.mu.Unlock()
		return
	}
	gb.closed = true
	cancel := gb.cancel
	close(gb.done)
	gb.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (gb *grokBackend) suppressNextClose() {
	if gb != nil {
		gb.suppressNextCloseFlag.Store(true)
	}
}

func (gb *grokBackend) markRateLimited() {
	if gb != nil {
		gb.rateLimited.Store(true)
	}
}

func (gb *grokBackend) killProcess() {
	if gb == nil {
		return
	}
	gb.mu.Lock()
	cancel := gb.cancel
	gb.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (gb *grokBackend) initFailed() bool {
	return gb.initErr.Load() != nil
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}
