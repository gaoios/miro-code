package app

import (
	"encoding/json"
	"strings"
	"time"
)

// looksLikeRateLimit reports whether a chunk of stderr text contains a
// recognizable rate-limit / 429 signature. Shared between the CC
// drainStderr (server_session.go) and the codex drainStderr
// (codex_backend.go) so the two stay in sync — adding a new signature
// here automatically benefits both backends.
//
// Caller is responsible for normalizing case (this function does not).
func looksLikeRateLimit(loweredChunk string) bool {
	return strings.Contains(loweredChunk, "429") ||
		strings.Contains(loweredChunk, "rate limit") ||
		strings.Contains(loweredChunk, "too many requests") ||
		strings.Contains(loweredChunk, "quota exceeded")
}

// SessionOpts is the unified options struct for spawning a backend session.
//
// It is intentionally a type alias for the existing sessionOpts so the CC
// implementation (claudeBackend) keeps using the same fields with no
// translation layer. Round 3's codexBackend will read the subset of fields
// it cares about (WorkDir, Model, ResumeID, ServerSessionID) and ignore
// CC-specific knobs (MCPConfig, ReplaceSoul, Chrome, MaxTurns).
type SessionOpts = sessionOpts

// BackendKind identifies which backend implementation a session is using.
//
// Adding a new backend kind is a matter of registering it here and providing
// a Backend implementation in its own file (e.g. codex_backend.go).
type BackendKind string

const (
	// BackendCC is the Claude Code stream-json subprocess backend.
	BackendCC BackendKind = "cc"
	// BackendCodex is the OpenAI codex app-server JSON-RPC backend (Round 3).
	BackendCodex BackendKind = "codex"
	// BackendGrok is the xAI Grok Build headless subprocess backend.
	BackendGrok BackendKind = "grok"
	// BackendKimi is the Moonshot Kimi CLI headless subprocess backend.
	BackendKimi BackendKind = "kimi"
	// BackendAgy is the Antigravity CLI headless subprocess backend.
	BackendAgy BackendKind = "agy"
	// BackendOpencode is the opencode CLI headless JSONL subprocess backend.
	BackendOpencode BackendKind = "opencode"
)

// BackendInfo describes a backend's static identity. The session manager
// surfaces these fields to the Web UI / IPC peers without needing to know
// which concrete backend produced them.
type BackendInfo struct {
	// Kind is the backend implementation kind.
	Kind BackendKind `json:"kind"`
	// Model is the model name the backend was launched with (e.g. "opus[1m]"
	// for CC, "gpt-5.1-codex-max" for codex).
	Model string `json:"model"`
	// SessionID is the backend-native session id. For CC this is the Claude
	// Code session id (UUID); for codex it will be the thread id.
	// May be empty until the backend reports its first init message.
	SessionID string `json:"session_id,omitempty"`
}

// Backend abstracts a long-running AI process driving one weiran session.
//
// The interface is sized to fit both CC's stream-json subprocess
// (claudeBackend) and codex's app-server JSON-RPC client (codexBackend in
// Round 3). Method names are deliberately lowercase so that:
//
//  1. Existing CC method names (alive, waitInit, sendMessage, …) satisfy
//     the interface as-is — the Round 1 refactor is a pure type change at
//     call sites (process *claudeProcess → process Backend) with zero
//     method-call rewrites.
//  2. The interface is package-private, which is correct: only same-package
//     code (claudeBackend, codexBackend, fakeBackend in tests) is allowed
//     to implement Backend.
//
// Implementations must be safe for concurrent use. In practice the session
// layer holds its own mutex and serializes most calls, but ad-hoc readers
// (heartbeat health checks, IPC peer alive checks) can call alive / info
// concurrently with everything else.
type Backend interface {
	// info returns the backend's static identity.
	info() BackendInfo

	// alive reports whether the underlying process is still running.
	// Must be safe to call on a nil receiver (returns false), matching the
	// existing claudeProcess.alive contract that several callers rely on.
	alive() bool

	// waitInit blocks until the backend is ready to accept user turns,
	// or returns false on timeout / process exit.
	waitInit(timeout time.Duration) bool

	// sendMessage writes a user turn (text with optional inline images).
	// Inline images use the markdown ![alt](url) syntax — backends are
	// responsible for fetching and packaging them per their own protocol
	// (CC: base64 image content blocks; codex: input_image items).
	sendMessage(content string) error

	// sendPermissionDecision answers an in-flight tool permission prompt.
	// For CC: writes a control_response keyed by the request_id. For codex
	// (Round 4): replies the matching */requestApproval JSON-RPC request.
	sendPermissionDecision(requestID string, decision map[string]any) error

	// controlRequest sends a fire-and-forget control message. Subtypes
	// used today: "interrupt", "set_model", "get_context_usage". Codex
	// translates these to thread/turn cancel + setModel notifications.
	controlRequest(subtype string, extra map[string]any) error

	// controlRequestSync sends a control request and waits for the matched
	// response or the timeout.
	controlRequestSync(subtype string, extra map[string]any, timeout time.Duration) (json.RawMessage, error)

	// shutdown gracefully terminates the backend (close stdin → SIGTERM →
	// SIGKILL for CC; thread/cancel + connection close for codex).
	shutdown()

	// suppressNextClose tells the bridge layer to skip the trailing "close"
	// SSE event when the next exit happens. Used during intentional reload
	// (chrome toggle, mode switch, model switch, soul reload) so the UI
	// doesn't see "Session ended." and immediately reconnect.
	suppressNextClose()

	// markRateLimited flags the backend as rate-limited. Called when a 429
	// or "rate limit" pattern is detected in real time (stderr drain) or
	// via stream-json result classification. The watchExit path inspects
	// this flag to choose between a fallback retry and a normal stopped
	// transition. Decoupling the setter behind a method keeps session.go
	// from reaching into CC-specific atomic fields.
	markRateLimited()

	// killProcess sends SIGKILL to the underlying process. Used by
	// ephemeral session rate-limit fallback paths to force-terminate when
	// a 429 is detected — CC sometimes exits cleanly on 429 otherwise,
	// which would skip the fallback chain.
	killProcess()
}
