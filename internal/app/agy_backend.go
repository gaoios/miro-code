package app

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

const agyEventsChanCapacity = 64

// agyBackend runs Agy CLI in bounded print mode, spawning one process per
// user turn while preserving a stable Soul server session identity.
type agyBackend struct {
	bin                        string
	cwd                        string
	model                      string
	mode                       string
	effort                     string
	dangerouslySkipPermissions bool
	sessionID                  string
	eventsCh                   chan UnifiedEvent
	done                       chan struct{}

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

func spawnAgy(opts SessionOpts) (*agyBackend, error) {
	bin := agyBinary
	if bin == "" {
		bin = "agy"
	}
	resolvedBin, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("agy binary %q not found: %w", bin, err)
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
		sessionID = fmt.Sprintf("agy-%d", time.Now().UnixNano())
	}
	return &agyBackend{
		bin:                        resolvedBin,
		cwd:                        cwd,
		model:                      agyResolveModel(opts.Model),
		mode:                       agyMode,
		effort:                     agyEffort,
		dangerouslySkipPermissions: agyDangerouslySkipPermissions,
		sessionID:                  sessionID,
		eventsCh:                   make(chan UnifiedEvent, agyEventsChanCapacity),
		done:                       make(chan struct{}),
	}, nil
}

func (kb *agyBackend) info() BackendInfo {
	if kb == nil {
		return BackendInfo{Kind: BackendAgy}
	}
	return BackendInfo{Kind: BackendAgy, Model: kb.model, SessionID: kb.sessionID}
}

func (kb *agyBackend) alive() bool {
	if kb == nil {
		return false
	}
	kb.mu.Lock()
	defer kb.mu.Unlock()
	return !kb.closed
}

func (kb *agyBackend) waitInit(time.Duration) bool {
	return kb != nil && kb.initErr.Load() == nil
}

func (kb *agyBackend) sendMessage(content string) error {
	if kb == nil {
		return fmt.Errorf("agy backend is nil")
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("empty message")
	}
	kb.mu.Lock()
	if kb.closed {
		kb.mu.Unlock()
		return fmt.Errorf("agy backend is shut down")
	}
	if kb.running {
		kb.mu.Unlock()
		return fmt.Errorf("agy turn already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	kb.running = true
	kb.cancel = cancel
	turnID := fmt.Sprintf("%s-turn-%d", kb.sessionID, kb.turnSeq.Add(1))
	kb.mu.Unlock()
	go kb.runTurn(ctx, turnID, content)
	return nil
}

func (kb *agyBackend) runTurn(ctx context.Context, turnID, content string) {
	var turnError string
	defer func() {
		kb.mu.Lock()
		kb.running = false
		kb.cancel = nil
		kb.mu.Unlock()
		if turnError != "" {
			kb.emitError(turnID, turnError)
		} else {
			kb.emit(UnifiedEvent{Kind: UEvtTurnCompleted, TurnID: turnID, Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "ok"})})
		}
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
		turnError = message
		return
	}
	text, err := parseAgyTextOutput(stdout)
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		if looksLikeRateLimit(strings.ToLower(message)) {
			kb.markRateLimited()
		}
		turnError = message
		return
	}
	itemID := turnID + "-item-1"
	kb.emit(UnifiedEvent{Kind: UEvtItemStarted, TurnID: turnID, ItemID: itemID, ItemKind: UItemAgentMessage, Payload: mustMarshalRaw(UnifiedItemPayload{})})
	kb.emit(UnifiedEvent{Kind: UEvtItemCompleted, TurnID: turnID, ItemID: itemID, ItemKind: UItemAgentMessage, Payload: mustMarshalRaw(UnifiedItemPayload{Result: mustMarshalRaw(map[string]any{"text": text})})})
}

func parseAgyTextOutput(raw []byte) (string, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "", fmt.Errorf("empty agy output")
	}
	return text, nil
}

func (kb *agyBackend) emitError(turnID, message string) {
	kb.emit(UnifiedEvent{Kind: UEvtBackendError, TurnID: turnID, Payload: mustMarshalRaw(map[string]any{"error": message})})
	kb.emit(UnifiedEvent{Kind: UEvtTurnCompleted, TurnID: turnID, Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "error", Error: message})})
}

func (kb *agyBackend) commandArgs(content string) []string {
	args := []string{"--print", content}
	if kb.model != "" {
		args = append(args, "--model", kb.model)
	}
	effort := kb.effort
	if effort == "" {
		effort = "low"
	}
	mode := kb.mode
	if mode == "" {
		mode = "plan"
	}
	args = append(args, "--effort", effort, "--mode", mode)
	if kb.dangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	args = append(args, "--print-timeout", "5m")
	return args
}

func (kb *agyBackend) events() <-chan UnifiedEvent {
	if kb == nil {
		ch := make(chan UnifiedEvent)
		close(ch)
		return ch
	}
	return kb.eventsCh
}

func (kb *agyBackend) emit(ev UnifiedEvent) {
	if kb == nil {
		return
	}
	select {
	case kb.eventsCh <- ev:
	case <-kb.done:
	}
}

func (kb *agyBackend) sendPermissionDecision(string, map[string]any) error {
	return fmt.Errorf("agy backend does not support permission decisions")
}

func (kb *agyBackend) controlRequest(subtype string, extra map[string]any) error {
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
		kb.model = agyResolveModel(model)
		kb.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("agy backend control request %q not supported", subtype)
	}
}

func (kb *agyBackend) controlRequestSync(subtype string, _ map[string]any, _ time.Duration) (json.RawMessage, error) {
	return nil, fmt.Errorf("agy backend control request %q not supported", subtype)
}

func (kb *agyBackend) shutdown() {
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

func (kb *agyBackend) suppressNextClose() {
	if kb != nil {
		kb.suppressNextCloseFlag.Store(true)
	}
}

func (kb *agyBackend) markRateLimited() {
	if kb != nil {
		kb.rateLimited.Store(true)
	}
}

func (kb *agyBackend) killProcess() {
	if kb != nil {
		_ = kb.controlRequest("interrupt", nil)
	}
}
