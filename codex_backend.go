package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// ── Codex backend (Round 3) ──
//
// codexBackend is the second Backend implementation, alongside claudeBackend
// in server_process.go. It drives an OpenAI `codex app-server` subprocess
// over JSON-RPC 2.0 (NDJSON over stdio) and translates codex's typed
// thread/turn/item event stream into the UnifiedEvent envelope defined in
// unified_events.go.
//
// Round 3 goals (Phase 3.3 + 3.4 + 3.5 of the codex backend plan):
//   - Spawn `codex app-server --listen stdio://` and complete the
//     initialize → initialized → thread/start handshake.
//   - Translate notifications (turn/started, item/started, item/*Delta,
//     item/completed, turn/completed, error) into UnifiedEvent payloads
//     emitted on cb.eventsCh.
//   - Provide stub handlers for the 3 server-initiated approval requests
//     (commandExec / fileChange / permissions). Round 3 default-declines
//     each one and emits a UEvtApproval for observability; Round 4 will
//     refactor this to route through the existing PreToolUse hook chain.
//   - Implement the full Backend interface so a future server_session.go
//     change (Round 4) can stash *codexBackend in sess.process without any
//     interface plumbing here.
//
// What Round 3 does NOT do:
//   - Wire codexBackend into server_session.go — that's Round 4 (mode +
//     backend fields + spawn config + model routing).
//   - Bridge approvals to the tool-hook PreToolUse chain — Round 4.
//   - Implement set_model — codex doesn't support mid-thread setModel,
//     so Round 4 will close + thread/resume the process to swap models.
//   - Replay events into sess.broadcaster (the SSE bridge) — Round 4.
//
// Concurrency model (mirrors claudeBackend):
//   - Goroutines spawned by spawnCodex:
//       1. drainStderr  — reads codex stderr, surfaces 429/rate-limit
//          patterns, forwards lines to logger.
//       2. watchProcessExit — `cmd.Wait()` then markDone().
//       3. client.Run  — JSON-RPC read+write loops.
//       4. watchClientDone — bridges client.Done() to cb.done so the
//          test path (no cmd) still observes transport close.
//       5. runHandshake — initialize → initialized → thread/start, then
//          signals initReady (or markDone on failure).
//   - eventsCh is bounded at 256; emit() is non-blocking and drops
//     overflow with a logger warning. Round 4's session bridge will
//     drain it; Round 3 tests drain it manually.
//   - All notification/request handlers run on the JSON-RPC client's
//     dispatch goroutines (one per inbound frame), so they may run
//     concurrently with one another and with sendMessage / shutdown.
//   - threadID / activeTurnID are atomic.Pointer[string] so any goroutine
//     can read the current ID without grabbing a mutex.

// ── Configuration constants ──

const (
	// codexHandshakeTimeout bounds the initialize → initialized →
	// thread/start sequence. 10s is generous; codex normally responds in
	// well under a second on local stdio. Failure here closes cb.done so
	// waitInit returns false promptly.
	codexHandshakeTimeout = 10 * time.Second

	// codexShutdownGracePeriod is how long shutdown() waits between
	// thread/unsubscribe → SIGTERM → SIGKILL.
	codexShutdownGracePeriod = 5 * time.Second

	// codexEventsChanCapacity is the buffer for unified events between the
	// notification handlers and the (Round 4) session bridge. 256 is the
	// same number used for the JSON-RPC outbound queue and is a comfortable
	// fit for one turn's worth of stream-deltas (a verbose `cat` of a 200-
	// line file produces ~200 deltas; a 5-tool-call turn ~50 events).
	codexEventsChanCapacity = 256

	// codexClientName / codexClientTitle identify weiran in codex's tracing
	// log. The protocol sends both — `name` is treated as a stable
	// identifier, `title` is human-readable.
	codexClientName  = "weiran"
	codexClientTitle = "weiran soul-cli"

	// codexDefaultPermissionProfile and codexDefaultApprovalPolicy match
	// the runbook recommendation: workspaceWrite + never approval policy
	// puts ALL approval governance into weiran's hook layer (Round 4 wires
	// this up). For Round 3 the codex side default-declines anything that
	// would otherwise prompt, so we won't see real approvals fire unless a
	// caller forces a tool that needs one.
	//
	// The name is retained for config compatibility (agents.codex.
	// permission_profile); on the wire it maps to sandbox:"workspace-write"
	// via codexSandboxMode.
	codexDefaultPermissionProfile = "workspaceWrite"
	codexDefaultApprovalPolicy    = "never"
)

// codexDefaultApprovalWaitTimeout caps how long handleApprovalRequest
// blocks waiting for sendPermissionDecision (or the bridge's hook decision)
// to reply. Default-decline fires on expiry so the codex turn never
// deadlocks. 30s matches CC's sendPermissionDecision practical SLA — long
// enough for the hook chain (in-process) plus user hooks (ms) plus a
// network-bound AskUserQuestion (multi-second).
//
// Per-backend overrides live on codexBackend.approvalWaitTimeout so tests
// can shorten the timeout per-instance instead of mutating a package var
// (which broke under t.Parallel() / -count N).
const codexDefaultApprovalWaitTimeout = 30 * time.Second

// pendingCodexApproval is one in-flight approval request. The handler
// goroutine blocks on `reply`; sendPermissionDecision (or the timeout
// path inside handleApprovalRequest) writes the decision payload onto
// the channel. The handler then converts the generic decision map into
// the typed CodexCommandExecApprovalResponse / FileChangeApprovalResponse
// / PermissionsApprovalResponse and returns it to the JSON-RPC client.
type pendingCodexApproval struct {
	method string              // ServerReq* method this approval belongs to
	params json.RawMessage     // raw params for diagnostics if anything errors out
	reply  chan map[string]any // 1-buffered; handler reads, decision-sender writes
}

// ── Backend struct ──

// codexBackend wraps a `codex app-server` subprocess and a CodexJSONRPCClient.
// It is one of the two Backend implementations; the other is claudeBackend in
// server_process.go.
type codexBackend struct {
	// ctx / cancel are the backend-scoped context. Closing it cascades:
	//   - aborts any in-flight Call() (via the JSON-RPC client's ctx)
	//   - signals the cmd.CommandContext to send SIGKILL
	//   - exits the writeLoop on the JSON-RPC client
	ctx    context.Context
	cancel context.CancelFunc

	// cmd / stdin / stdout / stderr describe the spawned process. nil on
	// the test path (newCodexBackend without spawnCodex), where the test
	// supplies pre-made io.Pipe pairs to the JSON-RPC client directly.
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	// client is the JSON-RPC 2.0 NDJSON transport over stdout/stdin. Set
	// either by spawnCodex (production) or directly by tests before
	// calling registerHandlers + runHandshake.
	client *CodexJSONRPCClient

	// model is the codex model string passed to thread/start. Round 4 will
	// remap weiran's "opus[1m]" → "gpt-5.1-codex-max" via config; here we
	// pass through whatever the caller gave us.
	model string
	// cwd is the working directory codex's commandExecution items run in
	// (and where it resolves relative file paths).
	cwd string
	// permissionProfile / approvalPolicy mirror the codex thread/start
	// parameters. Defaults are codexDefault* constants above; the caller
	// can override via SessionOpts in Round 4.
	permissionProfile string
	approvalPolicy    string

	// approvalWaitTimeout caps how long handleApprovalRequest blocks
	// waiting for a decision before default-declining. Initialized from
	// codexDefaultApprovalWaitTimeout; tests override per-instance.
	approvalWaitTimeout time.Duration

	// threadID is the codex thread.id captured from thread/start's response
	// (and re-confirmed by the thread/started notification). atomic.Pointer
	// so any handler can read without locking.
	threadID atomic.Pointer[string]
	// activeTurnID tracks the most recently started turn. Used by interrupt
	// to target the right turn without races.
	activeTurnID atomic.Pointer[string]

	// eventsCh carries UnifiedEvents to the session bridge. Buffered at
	// codexEventsChanCapacity; emit() is non-blocking and drops on overflow.
	eventsCh chan UnifiedEvent

	// initReady is closed after thread/start succeeds. waitInit blocks on
	// it; closes only once via initReadyOnce.
	initReady     chan struct{}
	initReadyOnce sync.Once
	// initErr captures the failure message if runHandshake aborted before
	// signalInit. Surfaced via info() for logging / fallback decisions.
	initErr atomic.Pointer[string]

	// done is closed when the codex process exits OR the JSON-RPC
	// transport closes (test path). doneOnce prevents double-close.
	done     chan struct{}
	doneOnce sync.Once
	exitCode atomic.Int32

	// rateLimited / suppressNextCloseFlag mirror the same atomic flags on
	// claudeBackend. The latter is consulted by Round 4's bridge to skip
	// "Session ended." SSE during intentional reload.
	rateLimited           atomic.Bool
	suppressNextCloseFlag atomic.Bool

	// lastTokenUsage caches the most recent thread/tokenUsage/updated
	// notification so controlRequestSync("get_context_usage", …) can
	// answer without round-tripping codex.
	tokenUsageMu   sync.Mutex
	lastTokenUsage *CodexThreadTokenUsage

	// logger is invoked for telemetry and debug output. Production path
	// writes to stderr with the appName prefix; tests can install a
	// per-test capturer.
	logger func(format string, args ...any)

	// shutdownOnce ensures shutdown() runs at most once.
	shutdownOnce sync.Once

	// itemKindByID retains the kind reported in item/started so the
	// matching item/completed (which only carries the item type, not the
	// kind we mapped) can re-derive its UnifiedItemKind.
	itemKindMu   sync.Mutex
	itemKindByID map[string]UnifiedItemKind

	// pendingApprovals tracks in-flight server-initiated approval requests
	// (Round 4 async pattern). Keyed by the synthetic approvalID we mint
	// in handleApprovalRequest and emit on UEvtApproval. The bridge layer
	// (server_process_codex.go) calls sendPermissionDecision(approvalID,
	// decision) once it has a verdict from the tool-hook chain or the
	// human; that decision is delivered onto the entry's reply channel,
	// the handler converts it to the typed codex response, and returns.
	approvalsMu      sync.Mutex
	pendingApprovals map[string]*pendingCodexApproval

	// soulPrompt is the assembled soul prompt content (SOUL.md + memory
	// + skills + …). Sourced from SessionOpts.SystemPromptFile. We route
	// it to one of two codex thread/start fields based on replaceSoul:
	//
	//   replaceSoul=false → developerInstructions (supplement mode)
	//                       Soul sits ON TOP of codex's built-in
	//                       baseInstructions. apply_patch grammar +
	//                       AGENTS.md spec + sandbox protocol all stay,
	//                       soul layer steers persona/tone. Default; matches
	//                       CC's --append-system-prompt-file behavior.
	//
	//   replaceSoul=true  → baseInstructions (replace mode)
	//                       Soul REPLACES codex's built-in 275-line prompt
	//                       wholesale. Loses apply_patch freeform grammar
	//                       guidance, preamble rules, etc. — caller is
	//                       responsible for re-supplying anything codex
	//                       needs. Matches CC's --system-prompt-file.
	//
	// Empty when no SystemPromptFile was passed (bare mode / tests); both
	// fields are then omitted from thread/start so codex uses its own
	// defaults.
	soulPrompt  string
	replaceSoul bool

	// resumeThreadID, when non-empty, makes runHandshake call
	// thread/resume with this thread id instead of thread/start.
	// Set from SessionOpts.ResumeID at construction time. Empty means a
	// fresh thread (current default behavior). Used by the rehydrate path
	// after server restart so codex sessions don't lose conversation
	// history. Codex 0.125.0+ persists threads in $CODEX_HOME by id.
	resumeThreadID string
}

// newCodexBackend constructs a codexBackend with default fields populated.
// The caller is responsible for setting cb.client, registering handlers, and
// starting the runtime goroutines. spawnCodex (production) and
// codex_backend_test.go (tests) both go through here.
func newCodexBackend(opts SessionOpts) *codexBackend {
	ctx, cancel := context.WithCancel(context.Background())
	cwd := opts.WorkDir
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	// Resolve model name through agents.codex.model_map (Round 4) so a
	// weiran-side identifier like "opus[1m]" can transparently route to
	// the configured codex model. codexResolveModel falls through to the
	// raw input when no mapping exists.
	model := codexResolveModel(opts.Model)

	// Resolve permission profile / approval policy from global config
	// (agents.codex.* in config.json), falling back to the package
	// constants if the loader didn't populate them (e.g. in tests).
	permissionProfile := codexPermissionProfile
	if permissionProfile == "" {
		permissionProfile = codexDefaultPermissionProfile
	}
	approvalPolicy := codexApprovalPolicy
	if approvalPolicy == "" {
		approvalPolicy = codexDefaultApprovalPolicy
	}

	// Read the soul prompt off disk if the caller staged one. Errors are
	// non-fatal: an unreadable file just means codex starts without the
	// soul layer, which is annoying but not session-breaking. We log via
	// stderr so the operator can spot the silent degradation in
	// `weiran log` / launchd.
	//
	// NOTE: cb.soulPrompt holds the SYNTHESIZED prompt — caller's body
	// from SystemPromptFile plus the "Runtime Environment" suffix appended
	// below. Don't expect byte-equality with the file contents on disk;
	// callers that round-trip through this field will see the suffix.
	var soulPrompt string
	if opts.SystemPromptFile != "" {
		if data, err := os.ReadFile(opts.SystemPromptFile); err == nil {
			soulPrompt = string(data)
		} else {
			fmt.Fprintf(os.Stderr, "[%s] codex: failed to read SystemPromptFile %q: %v (continuing without soul)\n",
				appName, opts.SystemPromptFile, err)
		}
	}

	// Runtime note: tell the agent which backend / model it's running on.
	// Without this, replace-mode (baseInstructions) sessions wipe codex's
	// built-in "I am Codex CLI" identity and the agent has no signal about
	// the harness underneath — leading to wrong self-reports like
	// "I'm GPT, not Codex" when asked. Append-mode (developerInstructions)
	// also benefits because codex's baseInstructions never names the JSON-RPC
	// harness explicitly. Append-only — keeps SOUL.md off the framework.
	if soulPrompt != "" {
		soulPrompt += fmt.Sprintf(
			"\n\n---\n\n# Runtime Environment\n\n"+
				"You are running on the **codex** backend (model: `%s`).\n"+
				"Your heart is OpenAI's codex app-server JSON-RPC harness, not Claude Code's stream-json subprocess.\n"+
				"When asked which core/backend/harness you're on, the answer is: codex (model %s).\n",
			model, model,
		)
	}

	return &codexBackend{
		ctx:                 ctx,
		cancel:              cancel,
		model:               model,
		cwd:                 cwd,
		permissionProfile:   permissionProfile,
		approvalPolicy:      approvalPolicy,
		approvalWaitTimeout: codexDefaultApprovalWaitTimeout,
		eventsCh:            make(chan UnifiedEvent, codexEventsChanCapacity),
		initReady:           make(chan struct{}),
		done:                make(chan struct{}),
		itemKindByID:        make(map[string]UnifiedItemKind),
		pendingApprovals:    make(map[string]*pendingCodexApproval),
		soulPrompt:          soulPrompt,
		replaceSoul:         opts.ReplaceSoul,
		resumeThreadID:      opts.ResumeID,
	}
}

// ── Spawn ──

// spawnCodex starts a `codex app-server --listen stdio://` subprocess and
// returns a *codexBackend with handshake in progress. The caller should
// call waitInit to block until ready.
//
// Naming follows server_process.go's spawnCC; both produce a Backend-impl
// pointer ready for handover to the session manager (in Round 4).
func spawnCodex(opts SessionOpts) (*codexBackend, error) {
	cb := newCodexBackend(opts)

	// CommandContext means cancel(cb.ctx) → SIGKILL the codex process.
	// That's the right behavior for both shutdown and ctx-driven cleanup.
	binary := codexBinary
	if binary == "" {
		binary = "codex"
	}
	cmd := exec.CommandContext(cb.ctx, binary, "app-server", "--listen", "stdio://")
	cmd.Dir = cb.cwd
	// Inherit env — codex relies on $HOME, $XDG_*, $CODEX_HOME, $OPENAI_API_KEY.
	cmd.Env = os.Environ()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cb.cancel()
		return nil, fmt.Errorf("codex stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		cb.cancel()
		return nil, fmt.Errorf("codex stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		cb.cancel()
		return nil, fmt.Errorf("codex stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		cb.cancel()
		return nil, fmt.Errorf("codex start: %w", err)
	}

	cb.cmd = cmd
	cb.stdin = stdin
	cb.stdout = stdout
	cb.stderr = stderr
	cb.logger = func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "[%s] codex: "+format+"\n", append([]any{appName}, args...)...)
	}

	cb.client = NewCodexJSONRPCClient(cb.ctx, stdout, stdin, WithJSONRPCLogger(cb.logger))
	cb.registerHandlers()

	go cb.drainStderr()
	go cb.watchProcessExit()
	go cb.client.Run()
	go cb.watchClientDone()
	go cb.runHandshake()

	return cb, nil
}

// ── Lifecycle helpers ──

// drainStderr reads codex's stderr, forwards lines to the logger, and flips
// rateLimited if it spots a 429 pattern. Mirrors server_session.go's
// drainStderr but lives on the backend so the session layer doesn't need to
// know which kind of stderr it's draining.
func (cb *codexBackend) drainStderr() {
	if cb.stderr == nil {
		return
	}
	buf := make([]byte, 4096)
	for {
		n, err := cb.stderr.Read(buf)
		if n > 0 {
			chunk := strings.ToLower(string(buf[:n]))
			if !cb.rateLimited.Load() && looksLikeRateLimit(chunk) {
				cb.rateLimited.Store(true)
				if cb.logger != nil {
					cb.logger("rate-limit detected in stderr; flagging backend")
				}
			}
			if cb.logger != nil {
				line := strings.TrimRight(string(buf[:n]), "\n")
				if line != "" {
					cb.logger("stderr: %s", line)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// watchProcessExit waits for codex to exit and triggers markDone. Production
// path only; the test path has no cmd.
func (cb *codexBackend) watchProcessExit() {
	if cb.cmd == nil {
		return
	}
	if err := cb.cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			cb.exitCode.Store(int32(exitErr.ExitCode()))
		} else {
			cb.exitCode.Store(1)
		}
	}
	cb.markDone()
}

// watchClientDone bridges the JSON-RPC client's Done channel and the
// backend's ctx to cb.done. This handles three exit paths uniformly:
//   - Real codex process exit  → stdin/stdout EOF → client.Run exits → client.Done closes.
//   - Test pipe-pair close      → same path as above.
//   - shutdown() called         → cb.cancel() fires cb.ctx.Done; we don't
//     wait for the read loop (which is blocked on a pipe scanner that may
//     never EOF in tests) before declaring the backend dead.
func (cb *codexBackend) watchClientDone() {
	select {
	case <-cb.client.Done():
	case <-cb.ctx.Done():
	}
	cb.markDone()
}

// markDone closes cb.done at most once. Also wakes a still-blocked waitInit
// by ensuring the init failure path completes.
func (cb *codexBackend) markDone() {
	cb.doneOnce.Do(func() {
		close(cb.done)
	})
}

// signalInit closes cb.initReady at most once.
func (cb *codexBackend) signalInit() {
	cb.initReadyOnce.Do(func() {
		close(cb.initReady)
	})
}

// failInit records the init failure, emits a backend_error UnifiedEvent,
// closes done so waitInit unblocks, and cancels the backend context.
func (cb *codexBackend) failInit(stage, msg string) {
	if cb.logger != nil {
		cb.logger("init failed at %s: %s", stage, msg)
	}
	combined := stage + ": " + msg
	cb.initErr.Store(&combined)
	recordCodexHandshake(false)
	cb.emit(UnifiedEvent{
		Kind: UEvtBackendError,
		Payload: mustMarshalRaw(map[string]any{
			"stage":  stage,
			"reason": msg,
		}),
	})
	cb.markDone()
	cb.cancel()
}

// ── Handshake ──

// runHandshake performs the initialize → initialized → thread/start
// sequence. Called once via `go cb.runHandshake()` after the JSON-RPC client
// is started.
func (cb *codexBackend) runHandshake() {
	ctx, cancel := context.WithTimeout(cb.ctx, codexHandshakeTimeout)
	defer cancel()

	version := buildVersion
	if version == "" {
		version = "dev"
	}

	initParams := &CodexInitializeParams{
		ClientInfo: CodexClientInfo{
			Name:    codexClientName,
			Title:   codexClientTitle,
			Version: version,
		},
	}
	if _, err := cb.client.Call(ctx, MethodInitialize, initParams); err != nil {
		cb.failInit("initialize", err.Error())
		return
	}

	if err := cb.client.Notify(MethodInitialized, nil); err != nil {
		cb.failInit("initialized", err.Error())
		return
	}

	// Build thread/start (or thread/resume) params via map[string]any so we
	// can selectively include the sandbox knob only when it resolves.
	// codexSandboxMode() translates the config aliases ("workspaceWrite" /
	// "readOnly" / "dangerFullAccess") into the SandboxMode enum that codex
	// 0.153.x expects. The default (codexDefaultPermissionProfile =
	// "workspaceWrite") maps to "workspace-write". Note the field is
	// `sandbox`, not the old `permissionProfile` object — see
	// codexSandboxMode's doc comment for the protocol history.
	params := map[string]any{
		"model":          cb.model,
		"cwd":            cb.cwd,
		"approvalPolicy": cb.approvalPolicy,
	}
	// Soul prompt routing — replaceSoul flag picks which codex slot.
	// Same in both start and resume paths: ThreadResumeParams accepts
	// baseInstructions / developerInstructions and applies them as the
	// system prompt for the resumed thread (existing turns keep their
	// original prompt; subsequent turns use the new one). This is
	// important for rehydrate after server restart when SOUL.md may have
	// been edited between runs.
	//
	//   replaceSoul=false → developerInstructions (supplement on top of
	//                       codex's 275-line baseInstructions). Tool-use
	//                       grammar / AGENTS.md spec / sandbox protocol
	//                       all stay; soul layers persona over them.
	//
	//   replaceSoul=true  → baseInstructions (full replacement of codex's
	//                       built-in prompt). Caller is on the hook for
	//                       any tool-use grammar codex would have supplied.
	//                       Mirrors CC's --system-prompt-file (本我 mode).
	//
	// Empty soulPrompt is omitted from both — codex's own defaults take
	// over (tests / bare mode rely on that).
	if cb.soulPrompt != "" {
		if cb.replaceSoul {
			params["baseInstructions"] = cb.soulPrompt
		} else {
			params["developerInstructions"] = cb.soulPrompt
		}
	}
	method := MethodThreadStart
	if cb.resumeThreadID != "" {
		method = MethodThreadResume
		params["threadId"] = cb.resumeThreadID
		// excludeTurns: keep the response light — we don't need the full
		// turn history surfaced back through the JSON-RPC reply, since the
		// session bridge replays its own JSONL log to the UI on rehydrate.
		params["excludeTurns"] = true
	}
	sandbox, sandboxErr := codexSandboxMode(cb.permissionProfile)
	if sandboxErr != nil {
		cb.failInit(method, sandboxErr.Error())
		return
	}
	if sandbox != "" {
		params["sandbox"] = sandbox
	}
	raw, err := cb.client.Call(ctx, method, params)
	if err != nil {
		// thread/resume can fail when the persisted thread on disk is gone
		// (older codex versions, $CODEX_HOME wiped, thread expired). Fall
		// back to thread/start so the session at least comes up — the bridge
		// will record the new thread id and the UI surfaces fresh history.
		// We log loudly so an operator notices the loss-of-context.
		if method == MethodThreadResume {
			if cb.logger != nil {
				cb.logger("thread/resume failed for %s: %v — falling back to thread/start (conversation history will be lost)",
					cb.resumeThreadID, err)
			}
			fmt.Fprintf(os.Stderr, "[%s] codex: thread/resume %s failed (%v); falling back to thread/start\n",
				appName, shortID(cb.resumeThreadID), err)
			// Strip every resume-only field before retrying as start.
			// Today that's just threadId + excludeTurns; if a future
			// codex version adds another resume-only param (e.g.
			// resumeFromTurn), it MUST be deleted here too — leaving it
			// in `params` will leak into thread/start where codex will
			// reject it.
			// TODO: factor into buildStartParams / buildResumeParams
			// helpers when a third resume-only field shows up so we
			// don't keep updating this delete list by hand.
			delete(params, "threadId")
			delete(params, "excludeTurns")
			method = MethodThreadStart
			raw, err = cb.client.Call(ctx, method, params)
		}
		if err != nil {
			cb.failInit(method, err.Error())
			return
		}
	}
	// ThreadStartResponse and ThreadResumeResponse share the same shape for
	// the fields we care about (thread.id, model). Decode through the start
	// type — extra fields in resume's response are silently ignored.
	var resp CodexThreadStartResponse
	if jerr := json.Unmarshal(raw, &resp); jerr != nil {
		cb.failInit(method+" decode", jerr.Error())
		return
	}
	threadID := resp.Thread.ID
	if threadID == "" {
		cb.failInit(method, "empty thread id in response")
		return
	}
	cb.threadID.Store(&threadID)
	cb.signalInit()
	recordCodexHandshake(true)
	if cb.logger != nil {
		verb := "started"
		if method == MethodThreadResume {
			verb = "resumed"
		}
		cb.logger("handshake complete: thread %s thread=%s model=%s", verb, threadID, resp.Model)
	}
}

// codexSandboxMode converts a string value (from
// agents.codex.permission_profile) into the SandboxMode enum that codex's
// thread/start and thread/resume expect.
//
// codex 0.153.x replaced the typed `permissionProfile` object with a plain
// `sandbox` enum. Sending `permissionProfile` now fails the handshake with
// -32602 "permissionProfile is no longer supported for thread/start; use
// `permissions` with a named profile id instead" — which, because the init
// error used to be swallowed, silently produced a session that did no work
// and still reported success.
//
// We deliberately do NOT take the `permissions` (named-profile) route that
// the error message suggests: it requires the experimentalApi capability,
// which our handshake does not request, and fails with -32600 without it.
// `sandbox` is the non-experimental field and is accepted by both
// thread/start and thread/resume.
//
// Recognized aliases (the config key keeps its historical name so existing
// agents.codex.permission_profile values keep working):
//
//   - ""                              → "" (omit; codex uses its own default,
//     which is a restricted read-only sandbox — the session appears "stuck"
//     on the first write. Set explicitly to avoid surprise.)
//   - "default" / "workspaceWrite" / "workspace-write" → "workspace-write"
//   - "readOnly" / "read-only"        → "read-only"
//   - "dangerFullAccess" / "danger-full-access" / "disabled" → "danger-full-access"
//
// Anything else is an error. We fail loudly rather than silently omitting the
// field: a dropped sandbox degrades to codex's read-only default, so the
// session stays alive but cannot write — precisely the silent-failure class
// this backend was just repaired for.
func codexSandboxMode(name string) (string, error) {
	s := strings.TrimSpace(name)
	switch s {
	case "":
		// Empty = let codex pick its own default. Conservative path; callers
		// who want write access should set "workspaceWrite".
		return "", nil
	case "default", "workspaceWrite", "workspace-write":
		return "workspace-write", nil
	case "readOnly", "read-only":
		return "read-only", nil
	case "dangerFullAccess", "danger-full-access", "disabled":
		return "danger-full-access", nil
	}
	if strings.HasPrefix(s, "{") {
		return "", fmt.Errorf(
			"agents.codex.permission_profile no longer accepts a JSON object (%q): codex 0.153.x replaced the PermissionProfile object with the SandboxMode enum. Use \"workspace-write\", \"read-only\", or \"danger-full-access\"",
			s)
	}
	return "", fmt.Errorf(
		"unrecognized agents.codex.permission_profile %q: expected \"default\"/\"workspaceWrite\" (→ workspace-write), \"readOnly\" (→ read-only), or \"dangerFullAccess\"/\"disabled\" (→ danger-full-access)",
		s)
}

// ── Backend interface ──

// info returns the backend's static identity. The session id (thread id)
// is best-effort: it's empty until thread/start succeeds.
func (cb *codexBackend) info() BackendInfo {
	out := BackendInfo{Kind: BackendCodex, Model: cb.model}
	if id := cb.threadID.Load(); id != nil {
		out.SessionID = *id
	}
	return out
}

// alive reports whether the backend is still running and able to accept new
// turns. False after process exit, transport close, or init failure.
// Safe on nil receiver (returns false).
func (cb *codexBackend) alive() bool {
	if cb == nil {
		return false
	}
	select {
	case <-cb.done:
		return false
	default:
		return true
	}
}

// waitInit blocks until thread/start completes, the backend dies, or the
// timeout expires. Returns true on success.
func (cb *codexBackend) waitInit(timeout time.Duration) bool {
	if cb == nil {
		return false
	}
	select {
	case <-cb.initReady:
		return true
	case <-cb.done:
		return false
	case <-time.After(timeout):
		return false
	}
}

// sendMessage sends a user turn. Markdown image references (![alt](url))
// are split out into separate CodexUserInput entries: text fragments stay
// as text; URLs become Image (remote) or LocalImage (local-fs path).
func (cb *codexBackend) sendMessage(content string) error {
	if cb == nil {
		return fmt.Errorf("codex backend: nil receiver")
	}
	select {
	case <-cb.done:
		return fmt.Errorf("codex backend: already exited")
	default:
	}
	threadIDPtr := cb.threadID.Load()
	if threadIDPtr == nil || *threadIDPtr == "" {
		return fmt.Errorf("codex backend: thread not initialized")
	}

	inputs := buildCodexUserInput(content)
	if len(inputs) == 0 {
		return nil
	}

	// Persist the user's turn before we hand off to codex so the transcript
	// reflects what was actually sent even if turn/start later errors.
	// Round 5+: write to a CC-shaped JSONL so /api/history/{id}/messages,
	// FTS, and rename all reuse the existing parsers.
	writeCodexUserMessage(*threadIDPtr, content)

	ctx, cancel := context.WithTimeout(cb.ctx, 30*time.Second)
	defer cancel()

	raw, err := cb.client.Call(ctx, MethodTurnStart, &CodexTurnStartParams{
		ThreadID: *threadIDPtr,
		Input:    inputs,
	})
	if err != nil {
		return fmt.Errorf("turn/start: %w", err)
	}
	var resp CodexTurnStartResponse
	if jerr := json.Unmarshal(raw, &resp); jerr != nil {
		return fmt.Errorf("decode turn/start: %w", jerr)
	}
	if resp.Turn.ID != "" {
		turnID := resp.Turn.ID
		cb.activeTurnID.Store(&turnID)
		cb.emit(UnifiedEvent{
			Kind:    UEvtTurnStarted,
			TurnID:  turnID,
			Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "running"}),
		})
	}
	return nil
}

// sendPermissionDecision delivers a hook-/UI-supplied decision for an
// approval request previously emitted as UEvtApproval. The handler
// goroutine inside handleApprovalRequest is blocked on the matching
// pendingCodexApproval.reply channel; we look it up by approvalID,
// remove it from the pending map (no double-deliveries possible), and
// hand off the decision payload non-blockingly.
//
// Decision shape mirrors CC's control_response payload to keep the
// bridge's hook-chain translation backend-agnostic:
//
//	map[string]any{
//	    "behavior":     "allow" | "deny",
//	    "message":      "<reason>",         // optional, used for deny
//	    "scope":        "turn" | "session", // optional, permissions only
//	}
//
// codexBackend converts these into the typed Codex* response inside the
// pending handler so callers don't have to know the codex protocol.
//
// Returns an error if the approval id is unknown (already replied to via
// timeout, or never seen) so the caller can log the dead drop. The handler
// path is unaffected — pending approvals always either get a real decision
// or trigger the timeout default-decline.
func (cb *codexBackend) sendPermissionDecision(requestID string, decision map[string]any) error {
	if cb == nil {
		return fmt.Errorf("codex backend: nil receiver")
	}
	cb.approvalsMu.Lock()
	pending, ok := cb.pendingApprovals[requestID]
	if ok {
		delete(cb.pendingApprovals, requestID)
	}
	cb.approvalsMu.Unlock()
	if !ok {
		return fmt.Errorf("codex backend: no pending approval %q (already decided or expired)", requestID)
	}
	// reply is buffered 1; this never blocks because no one else writes here.
	select {
	case pending.reply <- decision:
	default:
		if cb.logger != nil {
			cb.logger("sendPermissionDecision(req=%s) — reply channel was already written; dropping", requestID)
		}
	}
	return nil
}

// controlRequest dispatches fire-and-forget control verbs. Codex's protocol
// expresses these as JSON-RPC requests rather than control_request frames,
// so we Call() and discard the response in a goroutine to keep the method
// non-blocking like its CC counterpart.
func (cb *codexBackend) controlRequest(subtype string, extra map[string]any) error {
	if cb == nil {
		return fmt.Errorf("codex backend: nil receiver")
	}
	switch subtype {
	case "interrupt":
		threadIDPtr := cb.threadID.Load()
		turnIDPtr := cb.activeTurnID.Load()
		if threadIDPtr == nil || turnIDPtr == nil {
			return fmt.Errorf("codex: no active turn to interrupt")
		}
		threadID, turnID := *threadIDPtr, *turnIDPtr
		go func() {
			ctx, cancel := context.WithTimeout(cb.ctx, 5*time.Second)
			defer cancel()
			if _, err := cb.client.Call(ctx, MethodTurnInterrupt, &CodexTurnInterruptParams{
				ThreadID: threadID,
				TurnID:   turnID,
			}); err != nil && cb.logger != nil {
				cb.logger("turn/interrupt async error: %v", err)
			}
		}()
		return nil
	case "set_model":
		// Round 4 will refactor this to: shutdown current thread →
		// thread/resume with the new model. Codex doesn't support
		// changing model mid-thread; the only path is a re-spawn.
		return fmt.Errorf("codex: set_model not supported in Round 3")
	case "get_context_usage":
		// fire-and-forget variant — the data is already in our cache;
		// caller should use controlRequestSync if they need a value.
		return nil
	}
	return fmt.Errorf("codex: unknown control subtype %q", subtype)
}

// controlRequestSync is the synchronous variant. interrupt does a typed
// turn/interrupt Call; get_context_usage returns the cached usage; set_model
// is reserved for Round 4.
func (cb *codexBackend) controlRequestSync(subtype string, extra map[string]any, timeout time.Duration) (json.RawMessage, error) {
	if cb == nil {
		return nil, fmt.Errorf("codex backend: nil receiver")
	}
	switch subtype {
	case "interrupt":
		threadIDPtr := cb.threadID.Load()
		turnIDPtr := cb.activeTurnID.Load()
		if threadIDPtr == nil || turnIDPtr == nil {
			return nil, fmt.Errorf("codex: no active turn to interrupt")
		}
		ctx, cancel := context.WithTimeout(cb.ctx, timeout)
		defer cancel()
		return cb.client.Call(ctx, MethodTurnInterrupt, &CodexTurnInterruptParams{
			ThreadID: *threadIDPtr,
			TurnID:   *turnIDPtr,
		})
	case "set_model":
		return nil, fmt.Errorf("codex: set_model not supported in Round 3")
	case "get_context_usage":
		cb.tokenUsageMu.Lock()
		usage := cb.lastTokenUsage
		cb.tokenUsageMu.Unlock()
		if usage == nil {
			return json.RawMessage(`{}`), nil
		}
		return json.Marshal(usage)
	}
	return nil, fmt.Errorf("codex: unknown control subtype %q", subtype)
}

// shutdown terminates the backend gracefully:
//  1. send thread/unsubscribe so codex stops streaming notifications to us
//  2. close the JSON-RPC client (cancels its ctx; writeLoop drains)
//  3. SIGTERM the process; SIGKILL after grace period
func (cb *codexBackend) shutdown() {
	if cb == nil {
		return
	}
	cb.shutdownOnce.Do(func() {
		// Best-effort thread/unsubscribe with a short timeout. Failure is
		// fine — we kill the process anyway.
		if threadID := cb.threadID.Load(); threadID != nil && *threadID != "" {
			ctx, cancel := context.WithTimeout(cb.ctx, 2*time.Second)
			_, _ = cb.client.Call(ctx, MethodThreadUnsubscribe, &CodexThreadUnsubscribeParams{
				ThreadID: *threadID,
			})
			cancel()
		}
		if cb.client != nil {
			cb.client.Close()
		}
		// Production path only — kill the process.
		if cb.cmd != nil && cb.cmd.Process != nil {
			select {
			case <-cb.done:
				// Already exited.
			case <-time.After(codexShutdownGracePeriod):
				_ = cb.cmd.Process.Signal(syscall.SIGTERM)
			}
			select {
			case <-cb.done:
			case <-time.After(codexShutdownGracePeriod):
				_ = cb.cmd.Process.Kill()
			}
		}
		cb.cancel()
		// Guarantee callers that shutdown() returning implies alive()==false.
		// watchClientDone will also call markDone after observing ctx.Done(),
		// but it runs on another goroutine and may not have been scheduled by
		// the time shutdown() returns — under -race this manifested as a
		// flaky TestCodexBackendShutdown. markDone is idempotent (doneOnce).
		cb.markDone()
	})
}

// suppressNextClose flags the backend so the (Round 4) bridge skips the
// "Session ended." SSE event on the next exit. Mirrors claudeBackend.
func (cb *codexBackend) suppressNextClose() {
	if cb == nil {
		return
	}
	cb.suppressNextCloseFlag.Store(true)
}

// markRateLimited flips the rate-limit flag. drainStderr also flips it
// directly when it spots a 429; this method exists so session-layer code can
// flip it from outside without touching internal fields.
func (cb *codexBackend) markRateLimited() {
	if cb == nil {
		return
	}
	cb.rateLimited.Store(true)
}

// killProcess force-terminates the codex subprocess. Equivalent to
// claudeBackend.killProcess; used by ephemeral-session rate-limit fallback
// paths in Round 4.
func (cb *codexBackend) killProcess() {
	if cb == nil || cb.cmd == nil || cb.cmd.Process == nil {
		return
	}
	_ = cb.cmd.Process.Kill()
}

// ── Internal: notification handlers ──

// registerHandlers wires every codex notification + server request we care
// about to the JSON-RPC client. Called once before client.Run() starts.
func (cb *codexBackend) registerHandlers() {
	cb.client.OnNotification(NotifThreadStarted, cb.handleThreadStarted)
	cb.client.OnNotification(NotifThreadStatusChanged, cb.handleThreadStatusChanged)
	cb.client.OnNotification(NotifThreadClosed, cb.handleThreadClosed)
	cb.client.OnNotification(NotifThreadTokenUsage, cb.handleTokenUsage)

	cb.client.OnNotification(NotifTurnStarted, cb.handleTurnStarted)
	cb.client.OnNotification(NotifTurnCompleted, cb.handleTurnCompleted)
	cb.client.OnNotification(NotifTurnDiffUpdated, cb.handleTurnDiffUpdated)
	cb.client.OnNotification(NotifTurnPlanUpdated, cb.handleTurnPlanUpdated)

	cb.client.OnNotification(NotifItemStarted, cb.handleItemStarted)
	cb.client.OnNotification(NotifItemCompleted, cb.handleItemCompleted)
	cb.client.OnNotification(NotifAgentMessageDelta, cb.handleAgentMessageDelta)
	cb.client.OnNotification(NotifPlanDelta, cb.handlePlanDelta)
	cb.client.OnNotification(NotifReasoningSummaryDelta, cb.handleReasoningDelta)
	cb.client.OnNotification(NotifReasoningTextDelta, cb.handleReasoningDelta)
	cb.client.OnNotification(NotifCommandOutputDelta, cb.handleCommandOutputDelta)
	cb.client.OnNotification(NotifFileChangeOutputDelta, cb.handleFileChangeOutputDelta)
	cb.client.OnNotification(NotifFileChangePatchUpdated, cb.handleFileChangePatchUpdated)

	cb.client.OnNotification(NotifError, cb.handleErrorNotif)

	cb.client.OnRequest(ServerReqCommandExecApproval, cb.handleApprovalRequest(ServerReqCommandExecApproval))
	cb.client.OnRequest(ServerReqFileChangeApproval, cb.handleApprovalRequest(ServerReqFileChangeApproval))
	cb.client.OnRequest(ServerReqPermissionsApproval, cb.handleApprovalRequest(ServerReqPermissionsApproval))

	cb.client.SetDefaultRequestHandler(cb.handleUnknownServerRequest)
	cb.client.SetDefaultNotificationHandler(func(_ json.RawMessage) {
		// Codex emits many notifications we don't subscribe to (mcpServer/*,
		// model/loaded, etc.). Ignore silently — logging here would be
		// noisy at info level and useless above debug.
	})
}

func (cb *codexBackend) handleThreadStarted(params json.RawMessage) {
	var n CodexThreadStartedNotification
	if err := json.Unmarshal(params, &n); err != nil {
		if cb.logger != nil {
			cb.logger("thread/started decode: %v", err)
		}
		return
	}
	if n.Thread.ID != "" {
		cur := cb.threadID.Load()
		if cur == nil || *cur == "" {
			id := n.Thread.ID
			cb.threadID.Store(&id)
		}
	}
}

func (cb *codexBackend) handleThreadStatusChanged(params json.RawMessage) {
	var n CodexThreadStatusChangedNotification
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	if n.Status.Type == "systemError" && cb.logger != nil {
		cb.logger("thread status: systemError")
	}
}

func (cb *codexBackend) handleThreadClosed(params json.RawMessage) {
	if cb.logger != nil {
		cb.logger("thread closed by server")
	}
	// Closing the JSON-RPC client triggers our watchClientDone, which in
	// turn closes cb.done. That's enough to wake everyone blocked on us.
	if cb.client != nil {
		cb.client.Close()
	}
}

func (cb *codexBackend) handleTokenUsage(params json.RawMessage) {
	var n CodexThreadTokenUsageUpdatedNotification
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	cb.tokenUsageMu.Lock()
	usage := n.TokenUsage
	cb.lastTokenUsage = &usage
	cb.tokenUsageMu.Unlock()
}

func (cb *codexBackend) handleTurnStarted(params json.RawMessage) {
	var n CodexTurnStartedNotification
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	if n.Turn.ID != "" {
		id := n.Turn.ID
		cb.activeTurnID.Store(&id)
	}
	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnStarted,
		TurnID:  n.Turn.ID,
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "running"}),
		Raw:     params,
	})
}

func (cb *codexBackend) handleTurnCompleted(params json.RawMessage) {
	var n CodexTurnCompletedNotification
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	status := codexTurnStatusToUnified(n.Turn.Status)
	payload := UnifiedTurnPayload{Status: status}
	if n.Turn.Error != nil {
		payload.Error = n.Turn.Error.Message
	}
	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnCompleted,
		TurnID:  n.Turn.ID,
		Payload: mustMarshalRaw(payload),
		Raw:     params,
	})
	// Clear active turn — the next sendMessage starts fresh.
	cb.activeTurnID.Store(nil)
}

func (cb *codexBackend) handleTurnDiffUpdated(params json.RawMessage) {
	// Round 3: surfaced as a generic delta against an empty item id so
	// downstream consumers (Round 4 SSE bridge) can render diff aggregates
	// per turn. Codex doesn't tie diff updates to a specific item id.
	var n CodexTurnDiffUpdatedNotification
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	cb.emit(UnifiedEvent{
		Kind:      UEvtItemDelta,
		TurnID:    n.TurnID,
		DeltaType: "summary",
		Payload:   mustMarshalRaw(UnifiedDeltaPayload{Text: n.Diff}),
		Raw:       params,
	})
}

func (cb *codexBackend) handleTurnPlanUpdated(params json.RawMessage) {
	var n CodexTurnPlanUpdatedNotification
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	// Marshal the plan as the delta text so consumers can re-render.
	planJSON, _ := json.Marshal(n.Plan)
	cb.emit(UnifiedEvent{
		Kind:      UEvtItemDelta,
		TurnID:    n.TurnID,
		DeltaType: "plan",
		Payload:   mustMarshalRaw(UnifiedDeltaPayload{Text: string(planJSON)}),
		Raw:       params,
	})
}

func (cb *codexBackend) handleItemStarted(params json.RawMessage) {
	var n CodexItemStartedNotification
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	kind := codexItemKindToUnified(n.Item.Type)
	if n.Item.ID != "" {
		cb.itemKindMu.Lock()
		cb.itemKindByID[n.Item.ID] = kind
		cb.itemKindMu.Unlock()
	}
	payload := UnifiedItemPayload{Name: codexItemDisplayName(n.Item)}
	if len(n.Item.Arguments) > 0 {
		payload.Input = n.Item.Arguments
	}
	cb.emit(UnifiedEvent{
		Kind:     UEvtItemStarted,
		TurnID:   n.TurnID,
		ItemID:   n.Item.ID,
		ItemKind: kind,
		Payload:  mustMarshalRaw(payload),
		Raw:      params,
	})
}

func (cb *codexBackend) handleItemCompleted(params json.RawMessage) {
	var n CodexItemCompletedNotification
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	kind := codexItemKindToUnified(n.Item.Type)
	if n.Item.ID != "" {
		cb.itemKindMu.Lock()
		if k, ok := cb.itemKindByID[n.Item.ID]; ok {
			kind = k
			delete(cb.itemKindByID, n.Item.ID)
		}
		cb.itemKindMu.Unlock()
	}
	payload := UnifiedItemPayload{
		Name:    codexItemDisplayName(n.Item),
		IsError: n.Item.Status == "failed",
	}
	if n.Item.Text != "" || n.Item.AggregatedOutput != "" {
		text := n.Item.Text
		if text == "" {
			text = n.Item.AggregatedOutput
		}
		payload.Result = mustMarshalRaw(map[string]any{"text": text})
	} else if len(n.Item.Changes) > 0 {
		payload.Result = n.Item.Changes
	}
	cb.emit(UnifiedEvent{
		Kind:     UEvtItemCompleted,
		TurnID:   n.TurnID,
		ItemID:   n.Item.ID,
		ItemKind: kind,
		Payload:  mustMarshalRaw(payload),
		Raw:      params,
	})
}

func (cb *codexBackend) handleAgentMessageDelta(params json.RawMessage) {
	var n CodexAgentMessageDeltaNotification
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	cb.emit(UnifiedEvent{
		Kind:      UEvtItemDelta,
		TurnID:    n.TurnID,
		ItemID:    n.ItemID,
		ItemKind:  UItemAgentMessage,
		DeltaType: "text",
		Payload:   mustMarshalRaw(UnifiedDeltaPayload{Text: n.Delta}),
		Raw:       params,
	})
}

func (cb *codexBackend) handlePlanDelta(params json.RawMessage) {
	var n CodexPlanDeltaNotification
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	cb.emit(UnifiedEvent{
		Kind:      UEvtItemDelta,
		TurnID:    n.TurnID,
		ItemID:    n.ItemID,
		DeltaType: "plan",
		Payload:   mustMarshalRaw(UnifiedDeltaPayload{Text: n.Delta}),
		Raw:       params,
	})
}

func (cb *codexBackend) handleReasoningDelta(params json.RawMessage) {
	// Both summaryTextDelta and textDelta share the same wire shape
	// (threadId/turnId/itemId/delta + an index field we don't surface).
	var n CodexReasoningSummaryDeltaNotification
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	cb.emit(UnifiedEvent{
		Kind:      UEvtItemDelta,
		TurnID:    n.TurnID,
		ItemID:    n.ItemID,
		ItemKind:  UItemReasoning,
		DeltaType: "text",
		Payload:   mustMarshalRaw(UnifiedDeltaPayload{Text: n.Delta}),
		Raw:       params,
	})
}

func (cb *codexBackend) handleCommandOutputDelta(params json.RawMessage) {
	var n CodexCommandOutputDeltaNotification
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	cb.emit(UnifiedEvent{
		Kind:      UEvtItemDelta,
		TurnID:    n.TurnID,
		ItemID:    n.ItemID,
		ItemKind:  UItemCommandExec,
		DeltaType: "output",
		Payload:   mustMarshalRaw(UnifiedDeltaPayload{Output: n.Delta, Stream: "stdout"}),
		Raw:       params,
	})
}

func (cb *codexBackend) handleFileChangeOutputDelta(params json.RawMessage) {
	var n CodexFileChangeOutputDeltaNotification
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	cb.emit(UnifiedEvent{
		Kind:      UEvtItemDelta,
		TurnID:    n.TurnID,
		ItemID:    n.ItemID,
		ItemKind:  UItemFileChange,
		DeltaType: "output",
		Payload:   mustMarshalRaw(UnifiedDeltaPayload{Output: n.Delta, Stream: "stdout"}),
		Raw:       params,
	})
}

func (cb *codexBackend) handleFileChangePatchUpdated(params json.RawMessage) {
	var n CodexFileChangePatchUpdatedNotification
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	cb.emit(UnifiedEvent{
		Kind:      UEvtItemDelta,
		TurnID:    n.TurnID,
		ItemID:    n.ItemID,
		ItemKind:  UItemFileChange,
		DeltaType: "summary",
		Payload:   mustMarshalRaw(UnifiedDeltaPayload{Text: string(n.Changes)}),
		Raw:       params,
	})
}

func (cb *codexBackend) handleErrorNotif(params json.RawMessage) {
	var n CodexErrorNotification
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	cb.emit(UnifiedEvent{
		Kind:   UEvtBackendError,
		TurnID: n.TurnID,
		Payload: mustMarshalRaw(map[string]any{
			"reason":     n.Error.Message,
			"will_retry": n.WillRetry,
		}),
		Raw: params,
	})
}

// ── Internal: server-initiated request handlers (approvals) ──

// handleApprovalRequest builds a method-aware request handler that
// implements the Round 4 async approval pattern:
//
//  1. Register a pendingCodexApproval keyed by a synthetic approval id.
//  2. Emit UEvtApproval so the bridge layer (server_process_codex.go) can
//     run the tool-hook chain and call sendPermissionDecision.
//  3. Block waiting for either:
//     - a decision delivered onto pending.reply, OR
//     - codexApprovalWaitTimeout firing (default-decline), OR
//     - the backend ctx / done channel signaling shutdown (decline +
//     cleanup so we don't leak).
//  4. Translate the generic decision map into the typed Codex response
//     and return it. The JSON-RPC client serializes our return value back
//     to the codex process as the response payload for the open request.
//
// If sendPermissionDecision wins the race the pending entry is removed
// there. If the timeout fires first we remove the entry ourselves before
// returning so a late sendPermissionDecision call gets a clean
// "no pending approval" error instead of writing into a dropped channel.
func (cb *codexBackend) handleApprovalRequest(method string) JSONRPCRequestHandler {
	return func(params json.RawMessage) (any, error) {
		approvalID := uuid.NewString()
		subtype, toolName, useID, input := codexApprovalDigest(method, params)

		pending := &pendingCodexApproval{
			method: method,
			params: params,
			reply:  make(chan map[string]any, 1),
		}
		cb.approvalsMu.Lock()
		cb.pendingApprovals[approvalID] = pending
		cb.approvalsMu.Unlock()

		cb.emit(UnifiedEvent{
			Kind: UEvtApproval,
			Payload: mustMarshalRaw(UnifiedApproval{
				RequestID: approvalID,
				ToolName:  toolName,
				ToolUseID: useID,
				Input:     input,
				Subtype:   subtype,
			}),
			Raw: params,
		})
		if cb.logger != nil {
			cb.logger("approval %s id=%s: emitted UEvtApproval, waiting for decision", method, approvalID)
		}

		var decision map[string]any
		select {
		case decision = <-pending.reply:
			if cb.logger != nil {
				cb.logger("approval %s id=%s: decision received (behavior=%v)", method, approvalID, decision["behavior"])
			}
		case <-time.After(cb.approvalWaitTimeout):
			cb.approvalsMu.Lock()
			delete(cb.pendingApprovals, approvalID)
			cb.approvalsMu.Unlock()
			if cb.logger != nil {
				cb.logger("approval %s id=%s: timed out after %s; default-decline", method, approvalID, cb.approvalWaitTimeout)
			}
			return codexDefaultDecline(method), nil
		case <-cb.done:
			cb.approvalsMu.Lock()
			delete(cb.pendingApprovals, approvalID)
			cb.approvalsMu.Unlock()
			if cb.logger != nil {
				cb.logger("approval %s id=%s: backend exiting; decline", method, approvalID)
			}
			return codexDefaultDecline(method), nil
		case <-cb.ctx.Done():
			cb.approvalsMu.Lock()
			delete(cb.pendingApprovals, approvalID)
			cb.approvalsMu.Unlock()
			return codexDefaultDecline(method), nil
		}

		return codexDecisionToResponse(method, decision), nil
	}
}

// codexDecisionToResponse converts the generic CC-shaped decision map
// (behavior=allow|deny, message?, scope?) into the typed codex protocol
// response for the matching server request. Unknown shapes default-decline
// so the codex turn never deadlocks on a malformed reply from a hook.
func codexDecisionToResponse(method string, decision map[string]any) any {
	behavior, _ := decision["behavior"].(string)
	switch method {
	case ServerReqCommandExecApproval:
		if behavior == "allow" {
			return &CodexCommandExecApprovalResponse{Decision: CodexCommandExecApprovalAccept}
		}
		return &CodexCommandExecApprovalResponse{Decision: CodexCommandExecApprovalDecline}
	case ServerReqFileChangeApproval:
		if behavior == "allow" {
			return &CodexFileChangeApprovalResponse{Decision: CodexFileChangeApprovalAccept}
		}
		return &CodexFileChangeApprovalResponse{Decision: CodexFileChangeApprovalDecline}
	case ServerReqPermissionsApproval:
		// Permissions has no allow/deny — codex always wants a permissions
		// payload + scope. Pass through the requested permissions when the
		// hook chain says "allow"; otherwise reply with empty perms (no
		// new grants this turn). Scope defaults to "turn" — the safer
		// shorter window.
		scope, _ := decision["scope"].(string)
		if scope == "" {
			scope = CodexPermissionsScopeTurn
		}
		permsRaw := json.RawMessage(`{}`)
		if v, ok := decision["permissions"]; ok {
			if raw, err := json.Marshal(v); err == nil {
				permsRaw = raw
			}
		}
		return &CodexPermissionsApprovalResponse{
			Permissions: permsRaw,
			Scope:       scope,
		}
	}
	// Unknown method — return generic decline.
	return map[string]any{"decision": "decline"}
}

// handleUnknownServerRequest catches any */requestApproval or other
// server-initiated request we forgot to register. Returns a method-not-found
// error so codex handles the failure gracefully.
func (cb *codexBackend) handleUnknownServerRequest(params json.RawMessage) (any, error) {
	if cb.logger != nil {
		cb.logger("unhandled server request (params: %s)", truncate(string(params), 200))
	}
	return nil, &JSONRPCErrorObj{
		Code:    CodexJSONRPCErrMethodNotFound,
		Message: "weiran codex backend: handler not registered (Round 4 will add)",
	}
}

// ── Helpers ──

// emit is the non-blocking event publisher. On overflow (consumer slow or
// not yet attached) the OLDEST event is dropped, keeping the bus bounded.
// This matches the JSON-RPC client's outbound-queue policy.
func (cb *codexBackend) emit(e UnifiedEvent) {
	select {
	case cb.eventsCh <- e:
		return
	default:
	}
	// Full — drop oldest and try again.
	select {
	case <-cb.eventsCh:
		codexEventsDroppedTotal.Add(1)
		if cb.logger != nil {
			cb.logger("events channel overflow; dropped oldest event")
		}
	default:
	}
	select {
	case cb.eventsCh <- e:
	default:
		// Still full (concurrent producer). Drop the new one — caller
		// can't observe but the old data on the channel still matters.
		codexEventsDroppedTotal.Add(1)
		if cb.logger != nil {
			cb.logger("events channel still full; dropped new event")
		}
	}
}

// events returns the read-only side of the unified events channel. Round 4
// will plumb this into sess.broadcaster; Round 3 tests consume directly.
func (cb *codexBackend) events() <-chan UnifiedEvent {
	return cb.eventsCh
}

// buildCodexUserInput converts a markdown-style content string into the
// []CodexUserInput shape codex expects on turn/start. ![alt](URL) image
// references become Image (remote) or LocalImage (filesystem path); plain
// text fragments are returned as Text entries in source order.
//
// Mirrors buildMessageContent in server_process.go but produces codex's
// typed input array rather than CC's content blocks.
func buildCodexUserInput(content string) []CodexUserInput {
	if content == "" {
		return nil
	}
	matches := msgImageRe.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return []CodexUserInput{{Type: CodexUserInputText, Text: content}}
	}
	var out []CodexUserInput
	last := 0
	for _, m := range matches {
		if m[0] > last {
			text := strings.TrimSpace(content[last:m[0]])
			if text != "" {
				out = append(out, CodexUserInput{Type: CodexUserInputText, Text: text})
			}
		}
		imgURL := content[m[4]:m[5]]
		if strings.HasPrefix(imgURL, "http://") || strings.HasPrefix(imgURL, "https://") {
			out = append(out, CodexUserInput{Type: CodexUserInputImage, URL: imgURL})
		} else if strings.HasPrefix(imgURL, "/") || strings.HasPrefix(imgURL, "~/") {
			out = append(out, CodexUserInput{Type: CodexUserInputLocalImage, Path: imgURL})
		} else {
			// Unknown shape — pass it through as a remote URL to match
			// CC's permissive behavior. Codex will reject if invalid.
			out = append(out, CodexUserInput{Type: CodexUserInputImage, URL: imgURL})
		}
		last = m[1]
	}
	if last < len(content) {
		text := strings.TrimSpace(content[last:])
		if text != "" {
			out = append(out, CodexUserInput{Type: CodexUserInputText, Text: text})
		}
	}
	return out
}

// codexItemKindToUnified maps codex's tagged-union ItemType to the unified
// item kind enum. Unknown kinds default to ToolCall — that's the safest
// bucket; the SSE bridge can still render the item via Raw payload.
func codexItemKindToUnified(t CodexItemType) UnifiedItemKind {
	switch t {
	case CodexItemAgentMessage:
		return UItemAgentMessage
	case CodexItemReasoning:
		return UItemReasoning
	case CodexItemCommandExecution:
		return UItemCommandExec
	case CodexItemFileChange:
		return UItemFileChange
	case CodexItemMcpToolCall, CodexItemDynamicToolCall, CodexItemCollabAgentToolCall, CodexItemWebSearch:
		return UItemToolCall
	}
	return UItemToolCall
}

// codexItemDisplayName returns the most useful human-readable label for an
// item kind: a command line, file path, tool name, or empty for plain
// agent_message / reasoning.
func codexItemDisplayName(item CodexThreadItem) string {
	switch item.Type {
	case CodexItemCommandExecution:
		return item.Command
	case CodexItemMcpToolCall, CodexItemDynamicToolCall, CodexItemCollabAgentToolCall:
		if item.Tool != "" {
			return item.Tool
		}
	case CodexItemFileChange:
		return "fileChange"
	}
	return ""
}

// codexTurnStatusToUnified maps codex's TurnStatus string to the unified
// "ok" / "error" / "cancelled" trio. Unknown values fall through to "error"
// so consumers can react conservatively.
func codexTurnStatusToUnified(s string) string {
	switch s {
	case CodexTurnStatusCompleted:
		return "ok"
	case CodexTurnStatusInterrupted:
		return "cancelled"
	case CodexTurnStatusFailed:
		return "error"
	case CodexTurnStatusInProgress:
		return "running"
	}
	return "error"
}

// codexApprovalDigest extracts the most useful UI fields from an approval
// request payload — tool name, item id, and a JSON-encoded preview — so the
// emitted UEvtApproval carries enough context for a hook chain to act on.
func codexApprovalDigest(method string, params json.RawMessage) (subtype, toolName, useID string, input json.RawMessage) {
	switch method {
	case ServerReqCommandExecApproval:
		subtype = "permission"
		toolName = "Bash"
		var p CodexCommandExecApprovalParams
		if err := json.Unmarshal(params, &p); err == nil {
			useID = p.ItemID
			input, _ = json.Marshal(map[string]any{"command": p.Command, "cwd": p.Cwd, "reason": p.Reason})
		}
	case ServerReqFileChangeApproval:
		subtype = "permission"
		toolName = "FileChange"
		var p CodexFileChangeApprovalParams
		if err := json.Unmarshal(params, &p); err == nil {
			useID = p.ItemID
			input, _ = json.Marshal(map[string]any{"reason": p.Reason, "grantRoot": p.GrantRoot})
		}
	case ServerReqPermissionsApproval:
		subtype = "permission"
		toolName = "Permissions"
		var p CodexPermissionsApprovalParams
		if err := json.Unmarshal(params, &p); err == nil {
			useID = p.ItemID
			input, _ = json.Marshal(map[string]any{"cwd": p.Cwd, "reason": p.Reason, "permissions": p.Permissions})
		}
	default:
		subtype = "permission"
	}
	return
}

// codexDefaultDecline returns the typed decline response for each approval
// method. Used by the Round 3 default policy and surfaced in the test
// suite. Round 4 replaces this with a hook-driven decision.
func codexDefaultDecline(method string) any {
	switch method {
	case ServerReqCommandExecApproval:
		return &CodexCommandExecApprovalResponse{Decision: CodexCommandExecApprovalDecline}
	case ServerReqFileChangeApproval:
		return &CodexFileChangeApprovalResponse{Decision: CodexFileChangeApprovalDecline}
	case ServerReqPermissionsApproval:
		return &CodexPermissionsApprovalResponse{
			Permissions: json.RawMessage(`{}`),
			Scope:       CodexPermissionsScopeTurn,
		}
	}
	return map[string]any{"decision": "decline"}
}

// mustMarshalRaw marshals v as JSON or returns null on failure. Used inside
// emit() where a marshal error is a programming bug, not a runtime error;
// returning null keeps the event flowing so consumers see the kind/turn id
// even if the payload is broken.
func mustMarshalRaw(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return raw
}
