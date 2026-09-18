package app

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ── Stream-JSON message types (Claude Code ↔ weiran server) ──

// StreamMessage is the envelope for all stream-json messages (peek type/subtype first).
type StreamMessage struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
}

// InitMessage is the first message from Claude Code after startup.
type InitMessage struct {
	Type           string      `json:"type"`
	Subtype        string      `json:"subtype"`
	CWD            string      `json:"cwd"`
	SessionID      string      `json:"session_id"`
	Tools          []string    `json:"tools"`
	MCPServers     []MCPServer `json:"mcp_servers"`
	Model          string      `json:"model"`
	PermissionMode string      `json:"permissionMode"`
}

// MCPServer describes a connected MCP server.
type MCPServer struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// AssistantMsg wraps a Claude assistant response.
type AssistantMsg struct {
	Type      string     `json:"type"`
	Message   APIMessage `json:"message"`
	SessionID string     `json:"session_id"`
	UUID      string     `json:"uuid,omitempty"`
}

// APIMessage is the inner message from Claude.
type APIMessage struct {
	Model      string         `json:"model"`
	ID         string         `json:"id"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	StopReason *string        `json:"stop_reason"`
	Usage      *TokenUsage    `json:"usage"`
}

// ContentBlock is a text/thinking/tool_use/tool_result block.
type ContentBlock struct {
	Type      string          `json:"type"` // text, thinking, tool_use, tool_result
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"`
}

// TokenUsage tracks token consumption.
type TokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ResultMessage signals the end of one conversation turn.
type ResultMessage struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	DurationMs   int     `json:"duration_ms"`
	NumTurns     int     `json:"num_turns"`
	Result       string  `json:"result"`
	StopReason   string  `json:"stop_reason"`
	SessionID    string  `json:"session_id"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// ── Claude Code process wrapper ──

// sessionOpts configures a new Claude Code subprocess.
type sessionOpts struct {
	WorkDir          string
	SystemPromptFile string
	MCPConfig        string
	Model            string
	MaxTurns         int
	Chrome           bool
	// ReplaceSoul — when true, pass --system-prompt-file instead of
	// --append-system-prompt-file. This strips CC's native system prompt
	// (intro, doing tasks, tone, output efficiency, session guidance,
	// auto-memory, MCP instructions, env info) and leaves only the soul
	// prompt. Server-side Anthropic safety blocks (injection defense,
	// privacy, copyright) are NOT affected — they're injected by the API,
	// not by CC. See conversation 2026-04-08 for source-code analysis.
	ReplaceSoul bool
	// ResumeID — Claude Code session ID to resume. When set, adds
	// --resume <id> so the new process inherits conversation history.
	// Used by setReplaceSoul to preserve context across mode toggle.
	ResumeID string
	// ServerSessionID — the server session ID, injected as
	// {APPNAME}_SESSION_ID env var for IPC CLI commands.
	ServerSessionID string

	// Backend selects which Backend implementation to spawn. Empty defaults
	// to BackendCC (current behavior). BackendCodex routes through
	// spawnCodex (Round 4). createSessionWithOpts uses resolveBackendKind
	// to derive this from the session opts + model + global config.
	Backend BackendKind
}

// claudeBackend wraps a running Claude Code subprocess with stream-json
// pipes. It is one of the two Backend implementations (the other is
// codexBackend in Round 3); both satisfy the Backend interface defined in
// backend.go via lowercase package-private methods.
type claudeBackend struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	done     chan struct{}
	exitCode int
	mu       sync.Mutex // protects stdin writes

	// model is the model name the process was launched with (from sessionOpts).
	// Set once at spawn time; read via info().
	model string

	// claudeSessionID holds the session id reported by CC's first init
	// message. atomic.Pointer so info() can read concurrently with the
	// init-message handler that calls setClaudeSessionID. nil until init.
	claudeSessionID atomic.Pointer[string]

	// initReady is closed when the process emits its first "init" message,
	// signaling it's ready to accept user messages on stdin.
	initReady chan struct{}

	// suppressClose, when true, makes bridgeStdout skip the trailing "close"
	// SSE event on process exit. Used during intentional reload (e.g. chrome
	// toggle) so the UI doesn't see "Session ended."
	suppressClose atomic.Bool

	// rateLimited is set when stderr detects a 429/rate-limit error in real time.
	// This allows watchExit to trigger model fallback even when the process exits
	// with code 0 (Claude Code handles 429 internally but still exits cleanly).
	rateLimited atomic.Bool

	// stderrTail captures the last 4KB of stderr for exit event classification
	// (e.g. detecting rate_limit / 429 errors for model fallback).
	stderrTail limitedBuffer

	// Response waiters for synchronous control requests
	waitersMu sync.Mutex
	waiters   map[string]chan json.RawMessage
}

// spawnCC starts a Claude Code subprocess in stream-json mode and returns a
// fully-initialized *claudeBackend ready for waitInit + sendMessage.
//
// Naming: the Backend abstraction calls this "starting CC", matching the
// equivalent codex factory spawnCodex in Round 3. The previous name
// (spawnClaude) is preserved as a thin alias below so external in-tree
// references — notably the Haiku worker pool that holds *claudeBackend
// directly — keep compiling without churn.
func spawnCC(opts sessionOpts) (*claudeBackend, error) {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "bypassPermissions",
		// Route any `ask`-behavior permission checks (currently only
		// AskUserQuestion in bypassPermissions mode) through stdio
		// control_request/can_use_tool. Without this flag, AskUserQuestion
		// tool_use arrives on stdout but the subprocess auto-synthesizes
		// an is_error tool_result "Answer questions?" because no UI can
		// render the prompt. With it, claude waits on a control_response
		// carrying the allow/deny decision AND the user's answers in
		// updatedInput, which the Web UI produces via the answer-question
		// endpoint. Other tools stay auto-allowed by bypassPermissions,
		// so they never reach this stdio hop.
		"--permission-prompt-tool", "stdio",
	}

	if opts.SystemPromptFile != "" {
		if opts.ReplaceSoul {
			args = append(args, "--system-prompt-file", opts.SystemPromptFile)
		} else {
			args = append(args, "--append-system-prompt-file", opts.SystemPromptFile)
		}
	}
	if opts.MCPConfig != "" {
		args = append(args, "--mcp-config", opts.MCPConfig)
	}
	if opts.Model != "" {
		// For provider/model format (e.g. "zai/glm-5.1"), pass only the model name to --model
		args = append(args, "--model", providerModelName(opts.Model))
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(opts.MaxTurns))
	}
	if opts.Chrome {
		args = append(args, "--chrome")
	}
	if opts.ResumeID != "" {
		args = append(args, "--resume", opts.ResumeID)
	}

	cmd := exec.Command(claudeBin, args...)
	if opts.WorkDir != "" {
		cmd.Dir = opts.WorkDir
	} else {
		cmd.Dir = workspace
	}

	// Filter environment: remove CLAUDECODE to prevent nested detection
	env := filterEnv(os.Environ(), "CLAUDECODE")
	env = append(env, "CLAUDE_CODE_ENTRYPOINT=sdk-cli")
	// Use model-aware env injection: provider models route directly, Anthropic models use proxy
	env = injectProxyEnvWithModel(env, opts.ServerSessionID, opts.Model)

	// IPC env vars: inject session ID, server URL, and auth token so child
	// processes can use `{cli} session send/read/search` CLI commands.
	// Env var prefix is derived from appName (e.g. "weiran" → "WEIRAN_").
	prefix := ipcEnvPrefix()
	if opts.ServerSessionID != "" {
		env = append(env, prefix+"_SESSION_ID="+opts.ServerSessionID)
	}
	if serverURL := os.Getenv(prefix + "_SERVER_URL"); serverURL != "" {
		env = append(env, prefix+"_SERVER_URL="+serverURL)
	} else if isServerMode {
		// Self-referencing: build URL from server config
		env = append(env, fmt.Sprintf(prefix+"_SERVER_URL=http://127.0.0.1:%d", serverPort))
	}
	if authToken := os.Getenv(prefix + "_AUTH_TOKEN"); authToken != "" {
		env = append(env, prefix+"_AUTH_TOKEN="+authToken)
	} else if serverAuthToken != "" {
		env = append(env, prefix+"_AUTH_TOKEN="+serverAuthToken)
	}

	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	proc := &claudeBackend{
		cmd:        cmd,
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
		model:      opts.Model,
		done:       make(chan struct{}),
		initReady:  make(chan struct{}),
		waiters:    make(map[string]chan json.RawMessage),
		stderrTail: limitedBuffer{limit: 4096},
	}

	// Monitor process exit in background
	go func() {
		err := cmd.Wait()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				proc.exitCode = exitErr.ExitCode()
			} else {
				proc.exitCode = 1
			}
		}
		close(proc.done)
	}()

	return proc, nil
}

// signalInit marks the process as initialized and ready to accept user messages.
// Called by bridgeStdout when the init message is received. Safe to call multiple times.
func (p *claudeBackend) signalInit() {
	select {
	case <-p.initReady:
		// already signaled
	default:
		close(p.initReady)
	}
}

// waitInit blocks until the process emits its init message or the timeout expires.
// Returns true if init was received, false on timeout or process exit.
func (p *claudeBackend) waitInit(timeout time.Duration) bool {
	select {
	case <-p.initReady:
		return true
	case <-p.done:
		return false
	case <-time.After(timeout):
		return false
	}
}

// sendMessage writes a user message to Claude Code's stdin.
// If the message contains ![alt](url) image patterns, extracts images and
// sends a content array with text + image blocks (base64 encoded).
func (p *claudeBackend) sendMessage(content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case <-p.done:
		return fmt.Errorf("process already exited")
	default:
	}

	// Build content: detect image markdown and convert to content blocks
	msgContent := buildMessageContent(content)

	payload, _ := json.Marshal(map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": msgContent},
		"parent_tool_use_id": nil,
		"session_id":         "default",
	})
	_, err := p.stdin.Write(append(payload, '\n'))
	return err
}

// sendPermissionDecision writes a control_response answering an inbound
// can_use_tool control_request. Used when claude runs with
// --permission-prompt-tool stdio: a tool with ask-behavior permission
// (e.g. AskUserQuestion) triggers a control_request on stdout, and the
// subprocess blocks until we reply. For 'allow', pass updatedInput
// (already merged with the answers the user picked). For 'deny', pass
// a reason in message and leave updatedInput as nil.
func (p *claudeBackend) sendPermissionDecision(requestID string, decision map[string]any) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case <-p.done:
		return fmt.Errorf("process already exited")
	default:
	}

	payload, _ := json.Marshal(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   decision,
		},
	})
	_, err := p.stdin.Write(append(payload, '\n'))
	return err
}

// msgImageRe matches ![alt](url) image patterns in user messages.
var msgImageRe = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)

// buildMessageContent parses the message for image markdown and returns
// either a plain string (no images) or a content array (text + image blocks).
func buildMessageContent(content string) any {
	matches := msgImageRe.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return content // no images, keep as plain string
	}

	var blocks []map[string]any
	lastIdx := 0

	for _, match := range matches {
		// Text before this image
		if match[0] > lastIdx {
			text := strings.TrimSpace(content[lastIdx:match[0]])
			if text != "" {
				blocks = append(blocks, map[string]any{
					"type": "text",
					"text": text,
				})
			}
		}

		// Image URL
		imgURL := content[match[4]:match[5]]
		altText := content[match[2]:match[3]]

		imgBlock := resolveImageBlock(imgURL, altText)
		if imgBlock != nil {
			blocks = append(blocks, imgBlock)
		}

		lastIdx = match[1]
	}

	// Trailing text
	if lastIdx < len(content) {
		text := strings.TrimSpace(content[lastIdx:])
		if text != "" {
			blocks = append(blocks, map[string]any{
				"type": "text",
				"text": text,
			})
		}
	}

	if len(blocks) == 0 {
		return content
	}
	return blocks
}

// resolveImageBlock reads an image or PDF (local file or HTTP URL) and returns
// a Claude API content block with base64 data: "image" for images,
// "document" for application/pdf. Returns nil on failure.
func resolveImageBlock(imgURL string, altText string) map[string]any {
	var data []byte
	var mediaType string
	var err error

	if strings.HasPrefix(imgURL, "/uploads/") {
		// Local file: resolve against workspace uploads dir
		filename := filepath.Base(imgURL)
		localPath := filepath.Join(workspace, "uploads", filename)
		data, err = os.ReadFile(localPath)
		mediaType = guessMediaType(filename)
	} else if strings.HasPrefix(imgURL, "http://") || strings.HasPrefix(imgURL, "https://") {
		// Remote URL: download with timeout
		client := &http.Client{Timeout: 15 * time.Second}
		resp, respErr := client.Get(imgURL)
		if respErr != nil {
			fmt.Fprintf(os.Stderr, "[%s] image download failed: %v\n", appName, respErr)
			return nil
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "[%s] image download status: %d\n", appName, resp.StatusCode)
			return nil
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // 32MB max (PDFs can be larger than images)
		mediaType = resp.Header.Get("Content-Type")
		if mediaType == "" {
			mediaType = guessMediaType(imgURL)
		}
	} else {
		return nil // unsupported URL scheme
	}

	if err != nil || len(data) == 0 {
		fmt.Fprintf(os.Stderr, "[%s] image/document read failed: %v\n", appName, err)
		return nil
	}

	// PDFs become document blocks, everything else stays image blocks.
	blockType := "image"
	if strings.HasPrefix(mediaType, "application/pdf") {
		blockType = "document"
	}

	return map[string]any{
		"type": blockType,
		"source": map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       base64.StdEncoding.EncodeToString(data),
		},
	}
}

// guessMediaType returns a MIME type based on file extension.
func guessMediaType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	default:
		return "image/png"
	}
}

// controlRequest sends a control request (interrupt, set_model, context_usage, etc.).
func (p *claudeBackend) controlRequest(subtype string, extra map[string]any) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case <-p.done:
		return fmt.Errorf("process already exited")
	default:
	}

	req := map[string]any{"subtype": subtype}
	for k, v := range extra {
		req[k] = v
	}
	payload, _ := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": fmt.Sprintf("req_%d_%s", time.Now().UnixMilli(), randHex(4)),
		"request":    req,
	})
	_, err := p.stdin.Write(append(payload, '\n'))
	return err
}

// controlRequestSync sends a control request and waits for the matching response.
func (p *claudeBackend) controlRequestSync(subtype string, extra map[string]any, timeout time.Duration) (json.RawMessage, error) {
	reqID := fmt.Sprintf("req_%d_%s", time.Now().UnixMilli(), randHex(4))

	// Register waiter before sending
	ch := make(chan json.RawMessage, 1)
	p.waitersMu.Lock()
	p.waiters[reqID] = ch
	p.waitersMu.Unlock()

	defer func() {
		p.waitersMu.Lock()
		delete(p.waiters, reqID)
		p.waitersMu.Unlock()
	}()

	// Send request
	p.mu.Lock()
	select {
	case <-p.done:
		p.mu.Unlock()
		return nil, fmt.Errorf("process already exited")
	default:
	}
	req := map[string]any{"subtype": subtype}
	for k, v := range extra {
		req[k] = v
	}
	payload, _ := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": reqID,
		"request":    req,
	})
	_, err := p.stdin.Write(append(payload, '\n'))
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}

	// Wait for response
	select {
	case resp := <-ch:
		return resp, nil
	case <-p.done:
		return nil, fmt.Errorf("process exited while waiting")
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for response")
	}
}

// deliverResponse routes a control_response to the matching waiter (if any).
// Claude Code format: {"type":"control_response","response":{"subtype":"success","request_id":"req_xxx","response":{...}}}
// Returns true if a waiter consumed it.
func (p *claudeBackend) deliverResponse(raw json.RawMessage) bool {
	var peek struct {
		Response struct {
			RequestID string `json:"request_id"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &peek) != nil || peek.Response.RequestID == "" {
		return false
	}
	p.waitersMu.Lock()
	ch, ok := p.waiters[peek.Response.RequestID]
	p.waitersMu.Unlock()
	if ok {
		select {
		case ch <- raw:
		default:
		}
		return true
	}
	return false
}

// shutdown gracefully stops the Claude Code process.
// Close stdin → wait 5s → SIGTERM → wait 5s → SIGKILL.
func (p *claudeBackend) shutdown() {
	p.mu.Lock()
	p.stdin.Close()
	p.mu.Unlock()

	select {
	case <-p.done:
		return
	case <-time.After(5 * time.Second):
		p.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-p.done:
		return
	case <-time.After(5 * time.Second):
		p.cmd.Process.Kill()
	}
	<-p.done
}

// info returns the backend's static identity. The session-id field is
// best-effort: the CC subprocess only emits its own session id on the
// first init message, so callers that need it before init landed should
// fall back to the persisted DB record (server_session.go does both).
func (p *claudeBackend) info() BackendInfo {
	if p == nil {
		return BackendInfo{Kind: BackendCC}
	}
	out := BackendInfo{Kind: BackendCC, Model: p.model}
	if sid := p.claudeSessionID.Load(); sid != nil {
		out.SessionID = *sid
	}
	return out
}

// setClaudeSessionID records the CC session id reported by the init
// message. Idempotent — repeated calls with the same value are no-ops;
// changes (rare, e.g. resume swapping ids) overwrite. Safe for concurrent
// readers via info().
func (p *claudeBackend) setClaudeSessionID(id string) {
	if p == nil || id == "" {
		return
	}
	p.claudeSessionID.Store(&id)
}

// markRateLimited flags this backend as rate-limited. Setting the atomic
// flag is also done directly by drainStderr (which holds *claudeBackend),
// but session-layer code that only sees the Backend interface goes through
// this method so the rateLimited atomic stays an internal CC field.
func (p *claudeBackend) markRateLimited() {
	if p == nil {
		return
	}
	p.rateLimited.Store(true)
}

// killProcess sends SIGKILL to the underlying CC process. Used by
// ephemeral session rate-limit fallback paths to force-terminate even
// when CC would otherwise exit cleanly on 429.
func (p *claudeBackend) killProcess() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
}

// suppressNextClose flags this backend so its SSE bridge skips the
// trailing "close" event on the next process exit. Used by reload paths
// (chrome / mode / model toggle) where the UI must NOT see "Session
// ended." between the old and new subprocess. Wraps the suppressClose
// atomic so session-layer code can stay backend-agnostic.
func (p *claudeBackend) suppressNextClose() {
	if p == nil {
		return
	}
	p.suppressClose.Store(true)
}

// alive returns true if the process hasn't exited yet.
// Safe to call on nil receiver (returns false).
func (p *claudeBackend) alive() bool {
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// readLines reads stdout line by line, parses JSON, and calls handler for each valid message.
// Non-JSON lines (e.g. [SandboxDebug]) are silently skipped.
// Blocks until stdout is closed (process exits or stdin closed).
func (p *claudeBackend) readLines(handler func(msgType string, raw json.RawMessage)) {
	scanner := bufio.NewScanner(p.stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB buffer

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Skip non-JSON lines
		if line[0] != '{' {
			continue
		}

		// Peek at type
		var peek StreamMessage
		if err := json.Unmarshal(line, &peek); err != nil {
			continue
		}

		// Make a copy for the handler (scanner reuses buffer)
		raw := make(json.RawMessage, len(line))
		copy(raw, line)

		handler(peek.Type, raw)
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] readLines scanner error: %v\n", appName, err)
	}
}

// spawnClaude is the legacy name for spawnCC. Kept as a thin alias so
// in-tree callers that hold *claudeBackend directly (e.g. haikuPool.ensureSession,
// session manager spawn paths) keep compiling without churn while Round 1
// extracts the Backend abstraction. Both names produce the exact same
// *claudeBackend; new code should prefer spawnCC for symmetry with the
// codex factory landing in Round 3.
func spawnClaude(opts sessionOpts) (*claudeBackend, error) {
	return spawnCC(opts)
}

// ── Helpers ──

// filterEnv returns env slice with entries matching prefix removed.
func filterEnv(env []string, prefix string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, prefix+"=") && !strings.HasPrefix(e, prefix+"_") {
			out = append(out, e)
		}
	}
	return out
}

// randHex returns n random hex characters as a string.
func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)[:n]
}
