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

const opencodeEventsChanCapacity = 64

// opencodeBackend runs opencode CLI in bounded JSONL mode, spawning one process per
// user turn while preserving a stable Soul server session identity.
type opencodeBackend struct {
	bin       string
	cwd       string
	model     string
	sessionID string

	// Keep the CLI resume token separate from Soul's stable identity. The
	// event bridge uses sessionID for transcripts; opencode owns ses_ IDs.
	nativeSessionID string
	eventsCh        chan UnifiedEvent
	done            chan struct{}

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

func spawnOpencode(opts SessionOpts) (*opencodeBackend, error) {
	bin := opencodeBinary
	if bin == "" {
		bin = "opencode"
	}
	resolvedBin, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("opencode binary %q not found: %w", bin, err)
	}
	cwd := opts.WorkDir
	if cwd == "" {
		cwd = workspace
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	sessionID := opts.ServerSessionID
	if sessionID == "" {
		sessionID = opts.ResumeID
	}
	if sessionID == "" {
		sessionID = fmt.Sprintf("opencode-%d", time.Now().UnixNano())
	}
	return &opencodeBackend{
		bin:             resolvedBin,
		cwd:             cwd,
		model:           opencodeResolveModel(opts.Model),
		sessionID:       sessionID,
		nativeSessionID: opts.ResumeID,
		eventsCh:        make(chan UnifiedEvent, opencodeEventsChanCapacity),
		done:            make(chan struct{}),
	}, nil
}

func (kb *opencodeBackend) info() BackendInfo {
	if kb == nil {
		return BackendInfo{Kind: BackendOpencode}
	}
	kb.mu.Lock()
	defer kb.mu.Unlock()
	return BackendInfo{Kind: BackendOpencode, Model: kb.model, SessionID: kb.sessionID}
}

func (kb *opencodeBackend) alive() bool {
	if kb == nil {
		return false
	}
	kb.mu.Lock()
	defer kb.mu.Unlock()
	return !kb.closed
}

func (kb *opencodeBackend) waitInit(time.Duration) bool {
	return kb != nil && kb.initErr.Load() == nil
}

func (kb *opencodeBackend) sendMessage(content string) error {
	if kb == nil {
		return fmt.Errorf("opencode backend is nil")
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("empty message")
	}
	kb.mu.Lock()
	if kb.closed {
		kb.mu.Unlock()
		return fmt.Errorf("opencode backend is shut down")
	}
	if kb.running {
		kb.mu.Unlock()
		return fmt.Errorf("opencode turn already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	kb.running = true
	kb.cancel = cancel
	turnID := fmt.Sprintf("%s-turn-%d", kb.sessionID, kb.turnSeq.Add(1))
	kb.mu.Unlock()
	go kb.runTurn(ctx, turnID, content)
	return nil
}

func (kb *opencodeBackend) runTurn(ctx context.Context, turnID, content string) {
	var turnError string
	defer func() {
		kb.mu.Lock()
		kb.running = false
		kb.cancel = nil
		kb.mu.Unlock()
		// A completion lets the caller submit the next turn immediately.
		// Release the running slot before publishing that notification.
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
	text, nativeSessionID := parseOpencodeJSONLOutput(stdout)
	// Even a failed turn can have allocated a CLI session before exiting.
	// Preserve that token so retrying does not silently start a new history.
	if nativeSessionID != "" {
		kb.mu.Lock()
		kb.nativeSessionID = nativeSessionID
		kb.mu.Unlock()
	}
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
	itemID := turnID + "-item-1"
	kb.emit(UnifiedEvent{Kind: UEvtItemStarted, TurnID: turnID, ItemID: itemID, ItemKind: UItemAgentMessage, Payload: mustMarshalRaw(UnifiedItemPayload{})})
	kb.emit(UnifiedEvent{Kind: UEvtItemCompleted, TurnID: turnID, ItemID: itemID, ItemKind: UItemAgentMessage, Payload: mustMarshalRaw(UnifiedItemPayload{Result: mustMarshalRaw(map[string]any{"text": text})})})
}

// parseOpencodeJSONLOutput extracts assistant text and the CLI resume token.
// Unknown events and malformed lines are ignored so CLI diagnostics or newly
// introduced event types do not turn a successful process into a failed turn.
// Split the already-buffered output rather than using Scanner's default token
// limit: a single text event can contain an entire answer larger than 64 KiB.
func parseOpencodeJSONLOutput(raw []byte) (string, string) {
	var text strings.Builder
	var sessionID string
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var event struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionID"`
			Part      struct {
				Text string `json:"text"`
			} `json:"part"`
		}
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		if event.SessionID != "" {
			sessionID = event.SessionID
		}
		if event.Type == "text" {
			text.WriteString(event.Part.Text)
		}
	}
	return text.String(), sessionID
}

func (kb *opencodeBackend) emitError(turnID, message string) {
	kb.emit(UnifiedEvent{Kind: UEvtBackendError, TurnID: turnID, Payload: mustMarshalRaw(map[string]any{"error": message})})
	kb.emit(UnifiedEvent{Kind: UEvtTurnCompleted, TurnID: turnID, Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "error", Error: message})})
}

func (kb *opencodeBackend) commandArgs(content string) []string {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	// --auto permits file edits in headless runs. The working directory is
	// set through cmd.Dir; opencode run has no per-turn work-dir flag.
	args := []string{"run", "--format", "json", "--auto"}
	if kb.model != "" {
		args = append(args, "-m", kb.model)
	}
	if kb.nativeSessionID != "" {
		args = append(args, "-s", kb.nativeSessionID)
	}
	return append(args, content)
}

func (kb *opencodeBackend) events() <-chan UnifiedEvent {
	if kb == nil {
		ch := make(chan UnifiedEvent)
		close(ch)
		return ch
	}
	return kb.eventsCh
}

func (kb *opencodeBackend) emit(ev UnifiedEvent) {
	if kb == nil {
		return
	}
	select {
	case kb.eventsCh <- ev:
	case <-kb.done:
	}
}

func (kb *opencodeBackend) sendPermissionDecision(string, map[string]any) error {
	return fmt.Errorf("opencode backend does not support permission decisions")
}

func (kb *opencodeBackend) controlRequest(subtype string, extra map[string]any) error {
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
		kb.model = opencodeResolveModel(model)
		kb.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("opencode backend control request %q not supported", subtype)
	}
}

func (kb *opencodeBackend) controlRequestSync(subtype string, _ map[string]any, _ time.Duration) (json.RawMessage, error) {
	return nil, fmt.Errorf("opencode backend control request %q not supported", subtype)
}

func (kb *opencodeBackend) shutdown() {
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

func (kb *opencodeBackend) suppressNextClose() {
	if kb != nil {
		kb.suppressNextCloseFlag.Store(true)
	}
}

func (kb *opencodeBackend) markRateLimited() {
	if kb != nil {
		kb.rateLimited.Store(true)
	}
}

func (kb *opencodeBackend) killProcess() {
	if kb != nil {
		_ = kb.controlRequest("interrupt", nil)
	}
}
