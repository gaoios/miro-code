package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestExtractImagesUserUpload covers the primary Web-UI resume path: a user
// sends a message with an inline image (via resolveImageBlock or a native
// multimodal client). The user message content is an array with text and a
// top-level {type:"image",source:{...}} block.
func TestExtractImagesUserUpload(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"text","text":"look at this"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
	]`)
	imgs := extractImages(raw)
	if len(imgs) != 1 {
		t.Fatalf("expected 1 image from top-level user upload, got %d", len(imgs))
	}
	if imgs[0].MediaType != "image/png" || imgs[0].Data != "AAAA" {
		t.Fatalf("unexpected image payload: %+v", imgs[0])
	}
}

// TestExtractImagesToolResult preserves the original behaviour: images
// nested inside a tool_result block (e.g. Read of a .png) are still extracted.
func TestExtractImagesToolResult(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"tool_result","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"BBBB"}}
		]}
	]`)
	imgs := extractImages(raw)
	if len(imgs) != 1 {
		t.Fatalf("expected 1 image from tool_result, got %d", len(imgs))
	}
	if imgs[0].MediaType != "image/jpeg" || imgs[0].Data != "BBBB" {
		t.Fatalf("unexpected image payload: %+v", imgs[0])
	}
}

// TestExtractImagesMixed covers a realistic Web-UI session where a single
// user turn contains both a pasted image (top-level) and a later tool_result
// image (e.g. a screenshot dropped back in).
func TestExtractImagesMixed(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"text","text":"hi"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
		{"type":"tool_result","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/webp","data":"CCCC"}}
		]}
	]`)
	imgs := extractImages(raw)
	if len(imgs) != 2 {
		t.Fatalf("expected 2 images, got %d", len(imgs))
	}
	if imgs[0].Data != "AAAA" || imgs[1].Data != "CCCC" {
		t.Fatalf("unexpected ordering/payload: %+v", imgs)
	}
}

// TestExtractImagesSkipsNonBase64 guards against accidentally surfacing
// reference-type image blocks (e.g. {source:{type:"url",...}}) as base64.
func TestExtractImagesSkipsNonBase64(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"image","source":{"type":"url","url":"https://example.com/x.png"}}
	]`)
	imgs := extractImages(raw)
	if len(imgs) != 0 {
		t.Fatalf("expected 0 images (non-base64 source), got %d", len(imgs))
	}
}

// TestSnapshotPendingAUQ_PermissionSynthesizesQuestions guards against a
// regression where a permission prompt's stored Input (the original tool
// input, e.g. {"file_path":"...","content":"..."}) leaked through the
// /state endpoint instead of the synthesized {"questions":[...]} payload
// the Web UI expects. Before the fix, refreshing the page while a
// permission prompt was pending caused loadSessionState to drop the prompt
// silently (p.input.questions was undefined), leaving the subprocess
// blocked with no UI to approve or deny it.
func TestSnapshotPendingAUQ_PermissionSynthesizesQuestions(t *testing.T) {
	sess := &serverSession{}
	sess.recordPendingAUQ(&pendingAUQEntry{
		RequestID: "req-1",
		ToolUseID: "tool-1",
		Input:     json.RawMessage(`{"file_path":"/tmp/x.txt","content":"hello"}`),
		Kind:      "permission",
		ToolName:  "Write",
	})

	snap := sess.snapshotPendingAUQ()
	if len(snap) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(snap))
	}
	entry := snap[0]
	if entry["kind"] != "permission" {
		t.Fatalf("kind: want permission, got %v", entry["kind"])
	}
	input, ok := entry["input"].(map[string]any)
	if !ok {
		t.Fatalf("input not a map[string]any: %T %#v", entry["input"], entry["input"])
	}
	qs, ok := input["questions"].([]map[string]any)
	if !ok || len(qs) != 1 {
		t.Fatalf("questions missing or wrong shape: %#v", input["questions"])
	}
	q := qs[0]
	if q["question"] != "Do you want to let Claude run Write?" {
		t.Errorf("question text: got %v", q["question"])
	}
	opts, ok := q["options"].([]map[string]any)
	if !ok || len(opts) != 2 {
		t.Fatalf("options missing or wrong shape: %#v", q["options"])
	}
	if opts[0]["label"] != "Yes, this time" || opts[1]["label"] != "No, cancel" {
		t.Errorf("option labels wrong: %v / %v", opts[0]["label"], opts[1]["label"])
	}
	// Yes-description must include a preview of the original tool input so
	// the user has enough context to decide.
	if desc, _ := opts[0]["description"].(string); desc == "" || desc == "(no arguments)" {
		t.Errorf("yes-description missing preview: %q", desc)
	}
}

// TestSnapshotPendingAUQ_AUQPassesInputThrough confirms the synthesis path
// only fires for permission entries — real AskUserQuestion tool calls
// already carry the questions/options schema in their Input and must not
// be reshaped.
func TestSnapshotPendingAUQ_AUQPassesInputThrough(t *testing.T) {
	sess := &serverSession{}
	original := json.RawMessage(`{"questions":[{"question":"pick","options":[{"label":"A"}]}]}`)
	sess.recordPendingAUQ(&pendingAUQEntry{
		RequestID: "req-2",
		ToolUseID: "tool-2",
		Input:     original,
		Kind:      "auq",
		ToolName:  "AskUserQuestion",
	})
	snap := sess.snapshotPendingAUQ()
	if len(snap) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(snap))
	}
	got, ok := snap[0]["input"].(json.RawMessage)
	if !ok {
		t.Fatalf("auq input not a json.RawMessage: %T", snap[0]["input"])
	}
	if string(got) != string(original) {
		t.Fatalf("auq input was reshaped:\nwant %s\ngot  %s", original, got)
	}
}

func TestSessionManager_MaxSessionsLimit(t *testing.T) {
	sm := newSessionManager(2, 30*time.Minute, 4*time.Hour)
	defer sm.shutdownAll()

	// Fill interactive slots
	sm.mu.Lock()
	sm.sessions["s1"] = &serverSession{ID: "s1", Category: CategoryInteractive}
	sm.sessions["s2"] = &serverSession{ID: "s2", Category: CategoryInteractive}
	sm.mu.Unlock()

	// Attempt to create a 3rd interactive session via createSessionWithOpts
	dir := t.TempDir()
	_, err := sm.createSessionWithOpts(sessionCreateOpts{
		Project:  dir,
		Category: CategoryInteractive,
	})
	if err == nil || !strings.Contains(err.Error(), "max interactive sessions limit reached") {
		t.Fatalf("expected max interactive sessions limit error, got: %v", err)
	}

	// Ephemeral session (e.g. heartbeat) should NOT be blocked by interactive maxSessions
	_, err = sm.createSessionWithOpts(sessionCreateOpts{
		Project:  dir,
		Category: CategoryHeartbeat,
	})
	if err != nil && strings.Contains(err.Error(), "max interactive sessions limit reached") {
		t.Fatalf("heartbeat session should not be blocked by maxSessions limit: %v", err)
	}
}

type mockProcessForDestroyTest struct {
	shutdownCalled bool
}

func (m *mockProcessForDestroyTest) alive() bool { return true }
func (m *mockProcessForDestroyTest) shutdown()   { m.shutdownCalled = true }
func (m *mockProcessForDestroyTest) sendMessage(string) error { return nil }
func (m *mockProcessForDestroyTest) events() <-chan UnifiedEvent { return nil }
func (m *mockProcessForDestroyTest) info() BackendInfo { return BackendInfo{} }
func (m *mockProcessForDestroyTest) waitInit(time.Duration) bool { return true }
func (m *mockProcessForDestroyTest) suppressNextClose() {}
func (m *mockProcessForDestroyTest) sendPermissionDecision(string, map[string]any) error { return nil }
func (m *mockProcessForDestroyTest) controlRequest(string, map[string]any) error { return nil }
func (m *mockProcessForDestroyTest) controlRequestSync(string, map[string]any, time.Duration) (json.RawMessage, error) { return nil, nil }
func (m *mockProcessForDestroyTest) killProcess() {}
func (m *mockProcessForDestroyTest) markRateLimited() {}

func TestDestroySession_DetachesProcessUnderLock(t *testing.T) {
	sm := newSessionManager(5, 30*time.Minute, 4*time.Hour)
	defer sm.shutdownAll()

	proc := &mockProcessForDestroyTest{}
	sess := &serverSession{
		ID:      "test-destroy-sess",
		process: proc,
	}

	sm.mu.Lock()
	sm.sessions[sess.ID] = sess
	sm.mu.Unlock()

	if err := sm.destroySession(sess.ID); err != nil {
		t.Fatalf("destroySession: %v", err)
	}

	// Process pointer on session must be cleared under lock
	sess.mu.Lock()
	remainingProc := sess.process
	sess.mu.Unlock()

	if remainingProc != nil {
		t.Errorf("sess.process was not cleared under lock, got: %v", remainingProc)
	}

	if !proc.shutdownCalled {
		t.Errorf("expected mockProcess.shutdown to be called")
	}
}
