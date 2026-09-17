package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── Test fixtures ──
//
// codexFakeServer wraps fakeServer (from codex_jsonrpc_test.go) with the
// state machine a real `codex app-server` would offer just enough to drive
// the codex backend through its handshake and a few approval / item paths.
// Keeping the state explicit (vs. inlining everything in a closure) lets
// individual tests override the model name, decline-or-approve approvals,
// and capture client→server frames for inspection.

type codexFakeServer struct {
	t        *testing.T
	mu       sync.Mutex
	threadID string
	turnID   string
	model    string

	// Optional capture channels for tests that want to inspect what the
	// client sent (e.g. the decline reply to an approval request, or the
	// turn/start params on sendMessage).
	calls         chan JSONRPCEnvelope // client → server requests + notifs
	clientReplies chan JSONRPCEnvelope // client → server responses (no method, has id+result)

	// Behavior overrides.
	skipHandshakeReply bool // if true, the server doesn't reply to initialize → tests waitInit timeout
	failResumeOnce     bool // if true, the first thread/resume returns a JSON-RPC error (for fallback testing)

	// lastThreadStart holds the decoded params of the most recent
	// thread/start so tests can assert on the wire contract.
	lastThreadStart map[string]any
}

// lastThreadStartParams returns the decoded params of the most recent
// thread/start, or nil if none was seen.
func (s *codexFakeServer) lastThreadStartParams() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastThreadStart
}

// handle services one client→server frame and returns any responses the
// fake should send back. It also pushes captured frames into the optional
// channels.
func (s *codexFakeServer) handle(env JSONRPCEnvelope) []JSONRPCEnvelope {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Capture for caller-side assertions.
	if s.calls != nil {
		// Non-blocking send so a slow test doesn't hang the server.
		select {
		case s.calls <- env:
		default:
		}
	}
	// Frames with an id and no method are responses TO the server (i.e.
	// client replying to an approval). Capture and don't reply further.
	if len(env.ID) > 0 && env.Method == "" {
		if s.clientReplies != nil {
			select {
			case s.clientReplies <- env:
			default:
			}
		}
		return nil
	}

	switch env.Method {
	case MethodInitialize:
		if s.skipHandshakeReply {
			return nil
		}
		return []JSONRPCEnvelope{{
			ID: env.ID,
			Result: mustRaw(map[string]any{
				"userAgent":      "codex/test",
				"codexHome":      "/tmp/codex-home",
				"platformFamily": "unix",
				"platformOs":     "darwin",
			}),
		}}
	case MethodInitialized:
		// Notification — no reply.
		return nil
	case MethodThreadStart:
		// Mirror codex 0.153.4's contract: the typed permissionProfile object
		// was removed from thread/start in favour of the `sandbox` enum.
		// Reject the old field exactly as the real server does, so a
		// regression to it fails the handshake here instead of silently in
		// production.
		_ = json.Unmarshal(env.Params, &s.lastThreadStart)
		if _, bad := s.lastThreadStart["permissionProfile"]; bad {
			return []JSONRPCEnvelope{{
				ID: env.ID,
				Error: &JSONRPCErrorObj{
					Code:    -32602,
					Message: "permissionProfile is no longer supported for thread/start; use `permissions` with a named profile id instead",
				},
			}}
		}
		s.threadID = "thr_test_" + nonceShort()
		modelName := s.model
		if modelName == "" {
			modelName = "gpt-test"
		}
		return []JSONRPCEnvelope{{
			ID: env.ID,
			Result: mustRaw(map[string]any{
				"thread": map[string]any{
					"id":            s.threadID,
					"modelProvider": "openai",
					"createdAt":     1,
					"updatedAt":     2,
					"status":        map[string]any{"type": "idle"},
					"cwd":           "/tmp",
				},
				"model":             modelName,
				"modelProvider":     "openai",
				"cwd":               "/tmp",
				"approvalPolicy":    "never",
				"approvalsReviewer": "local",
			}),
		}}
	case MethodThreadResume:
		if s.failResumeOnce {
			s.failResumeOnce = false
			return []JSONRPCEnvelope{{
				ID: env.ID,
				Error: &JSONRPCErrorObj{
					Code:    -32603,
					Message: "thread not found",
				},
			}}
		}
		// Reuse the threadId the client requested (mirrors real codex
		// behaviour of resuming an existing persisted thread by id).
		var p struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(env.Params, &p)
		if p.ThreadID != "" {
			s.threadID = p.ThreadID
		} else if s.threadID == "" {
			s.threadID = "thr_resume_test_" + nonceShort()
		}
		modelName := s.model
		if modelName == "" {
			modelName = "gpt-test"
		}
		return []JSONRPCEnvelope{{
			ID: env.ID,
			Result: mustRaw(map[string]any{
				"thread": map[string]any{
					"id":            s.threadID,
					"modelProvider": "openai",
					"createdAt":     1,
					"updatedAt":     2,
					"status":        map[string]any{"type": "idle"},
					"cwd":           "/tmp",
				},
				"model":             modelName,
				"modelProvider":     "openai",
				"cwd":               "/tmp",
				"approvalPolicy":    "never",
				"approvalsReviewer": "local",
			}),
		}}
	case MethodTurnStart:
		s.turnID = "turn_test_" + nonceShort()
		return []JSONRPCEnvelope{{
			ID: env.ID,
			Result: mustRaw(map[string]any{
				"turn": map[string]any{
					"id":     s.turnID,
					"status": "inProgress",
				},
			}),
		}}
	case MethodTurnInterrupt:
		return []JSONRPCEnvelope{{ID: env.ID, Result: mustRaw(map[string]any{})}}
	case MethodThreadUnsubscribe:
		return []JSONRPCEnvelope{{ID: env.ID, Result: mustRaw(map[string]any{})}}
	}
	// Unknown method — let the test know but don't error out.
	if s.t != nil {
		s.t.Logf("codexFakeServer: unhandled method %q", env.Method)
	}
	return nil
}

// startCodexBackend wires up a *codexBackend connected to a codexFakeServer
// and kicks off the handshake. Caller is responsible for calling closer()
// in test cleanup.
func startCodexBackend(t *testing.T, fs *codexFakeServer) (*codexBackend, io.Writer, func()) {
	t.Helper()
	return startCodexBackendWithOpts(t, fs, SessionOpts{Model: "gpt-test", WorkDir: "/tmp"})
}

// startCodexBackendWithOpts is the configurable version of startCodexBackend.
// Tests use it when they need to set ResumeID (rehydrate path) or ReplaceSoul
// (本我 mode) on the backend at construction time.
func startCodexBackendWithOpts(t *testing.T, fs *codexFakeServer, opts SessionOpts) (*codexBackend, io.Writer, func()) {
	t.Helper()

	cr, cw, sr, sw, closeAll := pipePair()

	cb := newCodexBackend(opts)
	// Buffer logs so test output stays clean — surface only on failure.
	var (
		logsMu sync.Mutex
		logs   strings.Builder
	)
	cb.logger = func(format string, args ...any) {
		logsMu.Lock()
		defer logsMu.Unlock()
		fmt.Fprintf(&logs, format+"\n", args...)
	}
	cb.client = NewCodexJSONRPCClient(cb.ctx, cr, cw, WithJSONRPCLogger(cb.logger))
	cb.registerHandlers()

	if fs.calls == nil {
		fs.calls = make(chan JSONRPCEnvelope, 64)
	}
	if fs.clientReplies == nil {
		fs.clientReplies = make(chan JSONRPCEnvelope, 16)
	}
	if fs.t == nil {
		fs.t = t
	}

	innerServer := &fakeServer{
		reader:  sr,
		writer:  sw,
		stop:    make(chan struct{}),
		handler: fs.handle,
	}
	go innerServer.run(t)
	go cb.client.Run()
	go cb.watchClientDone()
	go cb.runHandshake()

	closer := func() {
		close(innerServer.stop)
		cb.cancel()
		cb.client.Close()
		closeAll()
		if t.Failed() {
			logsMu.Lock()
			defer logsMu.Unlock()
			t.Logf("backend logs:\n%s", logs.String())
		}
	}
	return cb, sw, closer
}

// nonceShort returns a short hex id for test entities (thread/turn names).
func nonceShort() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)
}

// mustRaw marshals v and panics on failure (test-only helper).
func mustRaw(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// ── Tests ──

// TestCodexBackendStartHappyPath verifies the full handshake completes and
// info() reflects the server-assigned thread id.
// TestCodexSandboxMode pins the config-alias → SandboxMode mapping.
//
// codex 0.153.x replaced the typed `permissionProfile` object with the
// `sandbox` enum; each alias below has to land on a value the server accepts,
// and anything unrecognized has to ERROR rather than silently omit the field
// (an omitted sandbox falls back to codex's read-only default, which looks
// like a live session that cannot write — the silent-failure shape this
// mapping exists to prevent).
func TestCodexSandboxMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
		wantErr        bool
	}{
		{name: "empty omits", in: "", want: ""},
		{name: "workspaceWrite legacy alias", in: "workspaceWrite", want: "workspace-write"},
		{name: "workspace-write kebab alias", in: "workspace-write", want: "workspace-write"},
		{name: "default alias matches workspaceWrite", in: "default", want: "workspace-write"},
		{name: "readOnly legacy", in: "readOnly", want: "read-only"},
		{name: "read-only kebab", in: "read-only", want: "read-only"},
		{name: "dangerFullAccess legacy", in: "dangerFullAccess", want: "danger-full-access"},
		{name: "danger-full-access kebab", in: "danger-full-access", want: "danger-full-access"},
		{name: "disabled aliases dangerFullAccess", in: "disabled", want: "danger-full-access"},
		{name: "surrounding whitespace tolerated", in: "  workspaceWrite\n", want: "workspace-write"},
		{name: "unknown bare string errors", in: "what", wantErr: true},
		{name: "old JSON literal errors with a migration hint",
			in: `{"type":"disabled"}`, wantErr: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := codexSandboxMode(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got sandbox %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("sandbox mismatch: want %q, got %q", tc.want, got)
			}
		})
	}
}

// TestCodexThreadStartSendsSandboxNotPermissionProfile guards the wire
// contract directly: the fake server rejects `permissionProfile` the way
// codex 0.153.4 does, so a regression to the old field fails the handshake
// here rather than silently in production.
func TestCodexThreadStartSendsSandboxNotPermissionProfile(t *testing.T) {
	fs := &codexFakeServer{model: "gpt-codex-test"}
	cb, _, closer := startCodexBackend(t, fs)
	defer closer()

	if !cb.waitInit(2 * time.Second) {
		t.Fatalf("waitInit failed; initErr=%v", cb.initErr.Load())
	}

	params := fs.lastThreadStartParams()
	if params == nil {
		t.Fatal("no thread/start params captured")
	}
	if _, bad := params["permissionProfile"]; bad {
		t.Errorf("thread/start must not send the removed permissionProfile field; params=%v", params)
	}
	if got, _ := params["sandbox"].(string); got != "workspace-write" {
		t.Errorf("want sandbox=workspace-write, got %v (params=%v)", params["sandbox"], params)
	}
}

func TestCodexBackendStartHappyPath(t *testing.T) {
	fs := &codexFakeServer{model: "gpt-codex-test"}
	cb, _, closer := startCodexBackend(t, fs)
	defer closer()

	if !cb.waitInit(2 * time.Second) {
		t.Fatalf("waitInit failed; initErr=%v", cb.initErr.Load())
	}
	info := cb.info()
	if info.Kind != BackendCodex {
		t.Errorf("got Kind %q, want codex", info.Kind)
	}
	if info.Model != "gpt-test" {
		t.Errorf("got Model %q, want gpt-test", info.Model)
	}
	if !strings.HasPrefix(info.SessionID, "thr_test_") {
		t.Errorf("got SessionID %q, want thr_test_ prefix", info.SessionID)
	}
	if !cb.alive() {
		t.Error("alive should be true after init")
	}
}

// TestCodexBackendResumePath verifies that when SessionOpts.ResumeID is set
// the backend calls thread/resume with the existing threadId (not
// thread/start) so conversation history persists across server restart.
func TestCodexBackendResumePath(t *testing.T) {
	fs := &codexFakeServer{model: "gpt-codex-test"}
	const wantThreadID = "thr_existing_persisted_42"
	cb, _, closer := startCodexBackendWithOpts(t, fs, SessionOpts{
		Model:    "gpt-test",
		WorkDir:  "/tmp",
		ResumeID: wantThreadID,
	})
	defer closer()

	if !cb.waitInit(2 * time.Second) {
		t.Fatalf("waitInit failed; initErr=%v", cb.initErr.Load())
	}

	// Drain captured frames and confirm we saw thread/resume (not start).
	close(fs.calls)
	sawResume := false
	sawStart := false
	for env := range fs.calls {
		switch env.Method {
		case MethodThreadResume:
			sawResume = true
			var p struct {
				ThreadID     string `json:"threadId"`
				ExcludeTurns bool   `json:"excludeTurns"`
			}
			if err := json.Unmarshal(env.Params, &p); err != nil {
				t.Fatalf("decode resume params: %v", err)
			}
			if p.ThreadID != wantThreadID {
				t.Errorf("resume threadId mismatch: got %q want %q", p.ThreadID, wantThreadID)
			}
			if !p.ExcludeTurns {
				t.Error("expected excludeTurns=true to keep response light")
			}
		case MethodThreadStart:
			sawStart = true
		}
	}
	if !sawResume {
		t.Error("expected thread/resume to be called")
	}
	if sawStart {
		t.Error("thread/start should NOT be called when ResumeID is set")
	}

	// Backend should report the resumed thread id, not a fresh one.
	if got := cb.info().SessionID; got != wantThreadID {
		t.Errorf("info.SessionID = %q, want %q", got, wantThreadID)
	}
}

// TestCodexBackendResumeFallbackToStart verifies that if thread/resume errors
// (e.g. persisted thread missing on disk after $CODEX_HOME wipe), the backend
// falls back to thread/start so the session at least comes up — losing
// conversation history is a survivable degradation, init-failure is not.
func TestCodexBackendResumeFallbackToStart(t *testing.T) {
	fs := &codexFakeServer{model: "gpt-codex-test", failResumeOnce: true}
	cb, _, closer := startCodexBackendWithOpts(t, fs, SessionOpts{
		Model:    "gpt-test",
		WorkDir:  "/tmp",
		ResumeID: "thr_does_not_exist",
	})
	defer closer()

	if !cb.waitInit(2 * time.Second) {
		t.Fatalf("waitInit failed; initErr=%v", cb.initErr.Load())
	}
	// Fallback created a fresh thread with a different id (the fake's
	// thread/start handler mints "thr_test_*").
	got := cb.info().SessionID
	if got == "thr_does_not_exist" {
		t.Errorf("fallback should have minted a new thread id, got resume id %q", got)
	}
	if !strings.HasPrefix(got, "thr_test_") {
		t.Errorf("fallback should mint a fresh test thread id, got %q", got)
	}
}

// TestCodexBackendWaitInitTimeout verifies waitInit returns false when the
// server never replies to initialize.
func TestCodexBackendWaitInitTimeout(t *testing.T) {
	fs := &codexFakeServer{skipHandshakeReply: true}
	cb, _, closer := startCodexBackend(t, fs)
	defer closer()

	if cb.waitInit(150 * time.Millisecond) {
		t.Fatal("expected waitInit to time out")
	}
}

// TestCodexBackendSendMessage verifies sendMessage issues a turn/start with
// the right thread id + parsed input, captures the assigned turn id, and
// emits a UEvtTurnStarted.
func TestCodexBackendSendMessage(t *testing.T) {
	fs := &codexFakeServer{}
	cb, _, closer := startCodexBackend(t, fs)
	defer closer()

	if !cb.waitInit(2 * time.Second) {
		t.Fatal("init failed")
	}

	if err := cb.sendMessage("hello world"); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}

	// Look for the turn/start request in the captured calls.
	var turnStartParams CodexTurnStartParams
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case env := <-fs.calls:
			if env.Method != MethodTurnStart {
				continue
			}
			if err := json.Unmarshal(env.Params, &turnStartParams); err != nil {
				t.Fatalf("decode turn/start params: %v", err)
			}
			goto found
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("never observed turn/start call")
found:
	if turnStartParams.ThreadID == "" || !strings.HasPrefix(turnStartParams.ThreadID, "thr_test_") {
		t.Errorf("turn/start ThreadID = %q", turnStartParams.ThreadID)
	}
	if len(turnStartParams.Input) != 1 ||
		turnStartParams.Input[0].Type != CodexUserInputText ||
		turnStartParams.Input[0].Text != "hello world" {
		t.Errorf("turn/start Input = %+v", turnStartParams.Input)
	}

	// Drain events until we find UEvtTurnStarted.
	gotTurnStarted := false
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !gotTurnStarted {
		select {
		case ev := <-cb.events():
			if ev.Kind == UEvtTurnStarted {
				gotTurnStarted = true
				if !strings.HasPrefix(ev.TurnID, "turn_test_") {
					t.Errorf("UEvtTurnStarted TurnID = %q", ev.TurnID)
				}
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !gotTurnStarted {
		t.Error("never received UEvtTurnStarted")
	}

	// activeTurnID should be set.
	if got := cb.activeTurnID.Load(); got == nil || !strings.HasPrefix(*got, "turn_test_") {
		t.Errorf("activeTurnID not set after sendMessage")
	}
}

// TestCodexBackendApprovalDefaultDecline verifies that a server-initiated
// command-execution approval gets a decline response when no hook chain
// answers within the timeout, AND emits a UEvtApproval for observability.
//
// Round 4 changed the default-decline path from synchronous to timeout-
// based. The test compresses the timeout to 50ms so it can verify the
// behavior without waiting 30s.
func TestCodexBackendApprovalDefaultDecline(t *testing.T) {
	fs := &codexFakeServer{}
	cb, sw, closer := startCodexBackend(t, fs)
	defer closer()
	cb.approvalWaitTimeout = 100 * time.Millisecond

	if !cb.waitInit(2 * time.Second) {
		t.Fatal("init failed")
	}

	// Push an approval request from the server side.
	approvalReq := JSONRPCEnvelope{
		ID:     json.RawMessage(`"approval-1"`),
		Method: ServerReqCommandExecApproval,
		Params: mustRaw(map[string]any{
			"threadId": "thr_test",
			"turnId":   "turn_test",
			"itemId":   "item_test",
			"command":  "rm -rf /tmp/foo",
			"cwd":      "/tmp",
			"reason":   "test",
		}),
	}
	b, _ := json.Marshal(approvalReq)
	if _, err := sw.Write(append(b, '\n')); err != nil {
		t.Fatalf("server write: %v", err)
	}

	// Expect UEvtApproval emitted.
	gotApproval := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !gotApproval {
		select {
		case ev := <-cb.events():
			if ev.Kind == UEvtApproval {
				gotApproval = true
				var ua UnifiedApproval
				if err := json.Unmarshal(ev.Payload, &ua); err != nil {
					t.Fatalf("decode UnifiedApproval: %v", err)
				}
				if ua.ToolName != "Bash" {
					t.Errorf("UnifiedApproval.ToolName = %q, want Bash", ua.ToolName)
				}
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !gotApproval {
		t.Error("never received UEvtApproval")
	}

	// Expect a decline reply on the server side.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case env := <-fs.clientReplies:
			if string(env.ID) != `"approval-1"` {
				continue
			}
			var resp CodexCommandExecApprovalResponse
			if err := json.Unmarshal(env.Result, &resp); err != nil {
				t.Fatalf("decode decline: %v", err)
			}
			if resp.Decision != CodexCommandExecApprovalDecline {
				t.Errorf("got decision %q, want decline", resp.Decision)
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("never received decline reply")
}

// TestCodexBackendApprovalAcceptViaSendDecision exercises the Round 4 async
// pattern: the server pushes an approval, the bridge layer (simulated here
// by directly reading cb.events()) gets UEvtApproval with the synthetic
// approval id, calls cb.sendPermissionDecision(id, behavior=allow), and the
// codex protocol response on the wire encodes accept.
func TestCodexBackendApprovalAcceptViaSendDecision(t *testing.T) {
	fs := &codexFakeServer{}
	cb, sw, closer := startCodexBackend(t, fs)
	defer closer()

	if !cb.waitInit(2 * time.Second) {
		t.Fatal("init failed")
	}

	approvalReq := JSONRPCEnvelope{
		ID:     json.RawMessage(`"approval-2"`),
		Method: ServerReqCommandExecApproval,
		Params: mustRaw(map[string]any{
			"threadId": "thr_test", "turnId": "turn_test", "itemId": "item_test",
			"command": "ls /tmp", "cwd": "/tmp",
		}),
	}
	b, _ := json.Marshal(approvalReq)
	if _, err := sw.Write(append(b, '\n')); err != nil {
		t.Fatalf("server write: %v", err)
	}

	// Read UEvtApproval, extract approval id, hand back an "allow".
	var approvalID string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && approvalID == "" {
		select {
		case ev := <-cb.events():
			if ev.Kind == UEvtApproval {
				var ua UnifiedApproval
				if err := json.Unmarshal(ev.Payload, &ua); err != nil {
					t.Fatalf("decode UnifiedApproval: %v", err)
				}
				approvalID = ua.RequestID
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if approvalID == "" {
		t.Fatal("never received UEvtApproval id")
	}

	if err := cb.sendPermissionDecision(approvalID, map[string]any{"behavior": "allow"}); err != nil {
		t.Fatalf("sendPermissionDecision: %v", err)
	}

	// Expect accept on the wire.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case env := <-fs.clientReplies:
			if string(env.ID) != `"approval-2"` {
				continue
			}
			var resp CodexCommandExecApprovalResponse
			if err := json.Unmarshal(env.Result, &resp); err != nil {
				t.Fatalf("decode accept: %v", err)
			}
			if resp.Decision != CodexCommandExecApprovalAccept {
				t.Errorf("got decision %q, want accept", resp.Decision)
			}
			// Second sendPermissionDecision for the same id should error
			// (already removed from pending).
			if err := cb.sendPermissionDecision(approvalID, map[string]any{"behavior": "deny"}); err == nil {
				t.Errorf("second sendPermissionDecision should error, got nil")
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("never received accept reply")
}

// TestCodexBackendItemNotificationToUnified verifies that a complete item
// life-cycle (started → delta → completed) round-trips through the unified
// event channel with the right kinds and payloads.
func TestCodexBackendItemNotificationToUnified(t *testing.T) {
	fs := &codexFakeServer{}
	cb, sw, closer := startCodexBackend(t, fs)
	defer closer()

	if !cb.waitInit(2 * time.Second) {
		t.Fatal("init failed")
	}

	send := func(method string, params any) {
		out := JSONRPCEnvelope{Method: method, Params: mustRaw(params)}
		b, _ := json.Marshal(out)
		if _, err := sw.Write(append(b, '\n')); err != nil {
			t.Fatalf("server write: %v", err)
		}
	}

	// turn/started
	send(NotifTurnStarted, map[string]any{
		"threadId": "thr_t", "turn": map[string]any{"id": "turn_x", "status": "inProgress"},
	})
	// item/started (agent_message kind)
	send(NotifItemStarted, map[string]any{
		"threadId": "thr_t", "turnId": "turn_x",
		"item": map[string]any{"type": "agentMessage", "id": "item_a"},
	})
	// item/agentMessage/delta
	send(NotifAgentMessageDelta, map[string]any{
		"threadId": "thr_t", "turnId": "turn_x", "itemId": "item_a", "delta": "Hi",
	})
	send(NotifAgentMessageDelta, map[string]any{
		"threadId": "thr_t", "turnId": "turn_x", "itemId": "item_a", "delta": " there",
	})
	// item/completed
	send(NotifItemCompleted, map[string]any{
		"threadId": "thr_t", "turnId": "turn_x",
		"item": map[string]any{"type": "agentMessage", "id": "item_a", "text": "Hi there"},
	})
	// turn/completed
	send(NotifTurnCompleted, map[string]any{
		"threadId": "thr_t",
		"turn":     map[string]any{"id": "turn_x", "status": "completed"},
	})

	// Drain events. The codex JSON-RPC client dispatches each notification
	// in its own goroutine (see handleNotification in codex_jsonrpc.go),
	// so per-method ordering is best-effort, not strict — the test asserts
	// the SET of events seen rather than a strict sequence.
	const wantCount = 6
	deadline := time.Now().Add(3 * time.Second)
	got := make([]UnifiedEvent, 0, wantCount)
	for len(got) < wantCount && time.Now().Before(deadline) {
		select {
		case ev := <-cb.events():
			got = append(got, ev)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if len(got) < wantCount {
		t.Fatalf("got %d events, want %d. observed: %+v", len(got), wantCount, got)
	}

	// Verify the multi-set of (Kind, ItemKind) pairs.
	type tag struct {
		kind     UnifiedEventKind
		itemKind UnifiedItemKind
	}
	wantTags := map[tag]int{
		{UEvtTurnStarted, ""}:                       1,
		{UEvtItemStarted, UItemAgentMessage}:        1,
		{UEvtItemDelta, UItemAgentMessage}:          2,
		{UEvtItemCompleted, UItemAgentMessage}:      1,
		{UEvtTurnCompleted, ""}:                     1,
	}
	gotTags := make(map[tag]int)
	var deltaTexts []string
	for _, ev := range got {
		gotTags[tag{ev.Kind, ev.ItemKind}]++
		if ev.Kind == UEvtItemDelta {
			var d UnifiedDeltaPayload
			if err := json.Unmarshal(ev.Payload, &d); err == nil {
				deltaTexts = append(deltaTexts, d.Text)
			}
		}
	}
	for k, v := range wantTags {
		if gotTags[k] != v {
			t.Errorf("tag %+v count = %d, want %d. all tags: %v", k, gotTags[k], v, gotTags)
		}
	}
	// Both delta texts should be present (any order).
	wantDeltas := map[string]bool{"Hi": true, " there": true}
	for _, d := range deltaTexts {
		delete(wantDeltas, d)
	}
	if len(wantDeltas) > 0 {
		t.Errorf("missing delta texts: %v (got: %v)", wantDeltas, deltaTexts)
	}

	// turn/completed should clear activeTurnID.
	if cb.activeTurnID.Load() != nil {
		t.Errorf("activeTurnID should be cleared after turn/completed, got %v", *cb.activeTurnID.Load())
	}
}

// TestCodexBackendShutdown verifies shutdown sends thread/unsubscribe and
// closes the backend cleanly so alive() flips to false.
func TestCodexBackendShutdown(t *testing.T) {
	fs := &codexFakeServer{}
	cb, _, closer := startCodexBackend(t, fs)
	defer closer()

	if !cb.waitInit(2 * time.Second) {
		t.Fatal("init failed")
	}
	threadID := cb.info().SessionID
	if threadID == "" {
		t.Fatal("expected thread id after init")
	}

	// Drain pending calls so we can spot the unsubscribe explicitly.
	drainLoop := func() {
		for {
			select {
			case <-fs.calls:
			default:
				return
			}
		}
	}
	drainLoop()

	doneCh := make(chan struct{})
	go func() {
		cb.shutdown()
		close(doneCh)
	}()

	// Look for thread/unsubscribe within 1s.
	gotUnsub := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !gotUnsub {
		select {
		case env := <-fs.calls:
			if env.Method == MethodThreadUnsubscribe {
				var p CodexThreadUnsubscribeParams
				if err := json.Unmarshal(env.Params, &p); err == nil && p.ThreadID == threadID {
					gotUnsub = true
				}
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !gotUnsub {
		t.Error("shutdown didn't issue thread/unsubscribe")
	}

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown didn't return")
	}

	// alive must be false after shutdown.
	if cb.alive() {
		t.Error("alive should be false after shutdown")
	}
}

// TestCodexBackendAliveFalseOnTransportClose verifies that closing the
// JSON-RPC transport (the test-side equivalent of process exit) flips
// alive() to false.
func TestCodexBackendAliveFalseOnTransportClose(t *testing.T) {
	fs := &codexFakeServer{}
	cb, _, closer := startCodexBackend(t, fs)

	if !cb.waitInit(2 * time.Second) {
		t.Fatal("init failed")
	}
	if !cb.alive() {
		t.Fatal("alive should be true after init")
	}
	// Tear down the entire transport — pipe pair + server. The closer
	// closes the underlying io.Pipe ends so the JSON-RPC read loop sees
	// EOF and Run() exits, which in turn closes client.Done() and triggers
	// watchClientDone → markDone.
	closer()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !cb.alive() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("alive never flipped to false after transport close")
}

// TestCodexBackendBuildUserInputImages verifies that markdown image refs
// split into the right CodexUserInput entries.
func TestCodexBackendBuildUserInputImages(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []CodexUserInput
	}{
		{
			name: "plain text",
			in:   "hello",
			want: []CodexUserInput{{Type: CodexUserInputText, Text: "hello"}},
		},
		{
			name: "single remote image",
			in:   "look at ![cat](https://example.com/cat.png)",
			want: []CodexUserInput{
				{Type: CodexUserInputText, Text: "look at"},
				{Type: CodexUserInputImage, URL: "https://example.com/cat.png"},
			},
		},
		{
			name: "local image path",
			in:   "![](/uploads/foo.png) caption",
			want: []CodexUserInput{
				{Type: CodexUserInputLocalImage, Path: "/uploads/foo.png"},
				{Type: CodexUserInputText, Text: "caption"},
			},
		},
		{
			name: "two images",
			in:   "![](https://a.com/1.png)![](https://b.com/2.png)",
			want: []CodexUserInput{
				{Type: CodexUserInputImage, URL: "https://a.com/1.png"},
				{Type: CodexUserInputImage, URL: "https://b.com/2.png"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildCodexUserInput(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("len(got) = %d, want %d (got=%+v)", len(got), len(c.want), got)
			}
			for i := range c.want {
				if got[i].Type != c.want[i].Type ||
					got[i].Text != c.want[i].Text ||
					got[i].URL != c.want[i].URL ||
					got[i].Path != c.want[i].Path {
					t.Errorf("got[%d] = %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestCodexBackendInterruptUsesTurnID verifies controlRequestSync("interrupt")
// targets the active turn id captured during sendMessage.
func TestCodexBackendInterruptUsesTurnID(t *testing.T) {
	fs := &codexFakeServer{}
	cb, _, closer := startCodexBackend(t, fs)
	defer closer()

	if !cb.waitInit(2 * time.Second) {
		t.Fatal("init failed")
	}
	if err := cb.sendMessage("hello"); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	// Wait for activeTurnID to populate.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cb.activeTurnID.Load() != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Drain calls so we can spot the interrupt clean.
	for {
		select {
		case <-fs.calls:
			continue
		default:
		}
		break
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = ctx
	if _, err := cb.controlRequestSync("interrupt", nil, time.Second); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case env := <-fs.calls:
			if env.Method != MethodTurnInterrupt {
				continue
			}
			var p CodexTurnInterruptParams
			if err := json.Unmarshal(env.Params, &p); err != nil {
				t.Fatalf("decode interrupt params: %v", err)
			}
			if !strings.HasPrefix(p.TurnID, "turn_test_") {
				t.Errorf("interrupt TurnID = %q", p.TurnID)
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("never observed turn/interrupt call")
}

// TestCodexBackendErrorNotificationEmits verifies error notifications turn
// into UEvtBackendError UnifiedEvents.
func TestCodexBackendErrorNotificationEmits(t *testing.T) {
	fs := &codexFakeServer{}
	cb, sw, closer := startCodexBackend(t, fs)
	defer closer()

	if !cb.waitInit(2 * time.Second) {
		t.Fatal("init failed")
	}

	out := JSONRPCEnvelope{Method: NotifError, Params: mustRaw(map[string]any{
		"error":     map[string]any{"message": "internal explosion"},
		"willRetry": false,
		"threadId":  "thr_t",
		"turnId":    "turn_x",
	})}
	b, _ := json.Marshal(out)
	if _, err := sw.Write(append(b, '\n')); err != nil {
		t.Fatalf("server write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case ev := <-cb.events():
			if ev.Kind != UEvtBackendError {
				continue
			}
			var got map[string]any
			if err := json.Unmarshal(ev.Payload, &got); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if got["reason"] != "internal explosion" {
				t.Errorf("payload reason = %v", got["reason"])
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("never observed UEvtBackendError")
}
