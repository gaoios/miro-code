package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSpawnOpencodeDefaultsAndIdentity(t *testing.T) {
	fake := writeFakeOpencodeBinary(t, "#!/bin/sh\nexit 0\n")
	oldBinary, oldMap, oldWorkspace := opencodeBinary, opencodeModelMap, workspace
	t.Cleanup(func() { opencodeBinary, opencodeModelMap, workspace = oldBinary, oldMap, oldWorkspace })
	opencodeBinary, opencodeModelMap, workspace = "", nil, t.TempDir()
	t.Setenv("PATH", filepath.Dir(fake))
	kb, err := spawnOpencode(SessionOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer kb.shutdown()
	if kb.bin != fake || kb.cwd != workspace || kb.model != "" || !strings.HasPrefix(kb.info().SessionID, "opencode-") || !kb.alive() || !kb.waitInit(0) {
		t.Fatalf("unexpected defaults: %+v", kb.info())
	}
	opencodeBinary = fake
	opencodeModelMap = map[string]string{"worker": "opencode-go/deepseek-flash"}
	kb, err = spawnOpencode(SessionOpts{Model: "worker", WorkDir: t.TempDir(), ServerSessionID: "soul-id", ResumeID: "ses_existing"})
	if err != nil {
		t.Fatal(err)
	}
	defer kb.shutdown()
	if info := kb.info(); info.Kind != BackendOpencode || info.Model != "opencode-go/deepseek-flash" || info.SessionID != "soul-id" {
		t.Fatalf("bad backend identity: %+v", info)
	}
	if kb.nativeSessionID != "ses_existing" {
		t.Fatalf("native session = %q", kb.nativeSessionID)
	}
	opencodeBinary = filepath.Join(t.TempDir(), "missing-opencode")
	if _, err := spawnOpencode(SessionOpts{}); err == nil || !strings.Contains(err.Error(), opencodeBinary) {
		t.Fatalf("missing binary error = %v", err)
	}
}

func TestOpencodeCommandArgs(t *testing.T) {
	kb := &opencodeBackend{model: "opencode-go/deepseek-flash", nativeSessionID: "ses_resume", cwd: "/unused"}
	want := []string{"run", "--format", "json", "--auto", "-m", "opencode-go/deepseek-flash", "-s", "ses_resume", "hello world"}
	if got := kb.commandArgs("hello world"); !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
	kb.model, kb.nativeSessionID = "", ""
	want = []string{"run", "--format", "json", "--auto", "hello"}
	if got := kb.commandArgs("hello"); !reflect.DeepEqual(got, want) {
		t.Fatalf("default args = %q, want %q", got, want)
	}
}

func TestParseOpencodeJSONLOutput(t *testing.T) {
	raw := []byte("\nnot JSON\n" + `{"type":"step_start","sessionID":"ses_123"}` + "\n" +
		`{"type":"text","sessionID":"ses_123","part":{"text":"hello "}}` + "\n" +
		`{"type":"future_event","part":{"text":"ignored"}}` + "\n{bad\n" +
		`{"type":"text","part":{"text":"world\n"}}` + "\n" +
		`{"type":"step_finish","sessionID":"ses_123","part":{"tokens":{"total":42},"cost":0.0049}}`)
	text, sid := parseOpencodeJSONLOutput(raw)
	if text != "hello world\n" || sid != "ses_123" {
		t.Fatalf("parsed text=%q session=%q", text, sid)
	}
	if text, sid := parseOpencodeJSONLOutput([]byte("\n{bad\n")); text != "" || sid != "" {
		t.Fatalf("malformed-only output = %q, %q", text, sid)
	}
	// A text event may contain an entire long answer; avoid Scanner's 64 KiB limit.
	long := strings.Repeat("x", 100000)
	encoded, _ := json.Marshal(map[string]any{"type": "text", "part": map[string]string{"text": long}})
	if text, _ := parseOpencodeJSONLOutput(encoded); text != long {
		t.Fatal("long text event was truncated")
	}
}

func TestOpencodeBackendTurnsResumeAndEmitUnifiedEvents(t *testing.T) {
	fake := writeFakeOpencodeBinary(t, `#!/bin/sh
set -eu
printf '%s\n' "$@" >> "$OPENCODE_ARGS_FILE"
pwd > "$OPENCODE_CWD_FILE"
printf '%s\n' '{"type":"step_start","sessionID":"ses_native"}' 'bad JSON' '{"type":"text","sessionID":"ses_native","part":{"text":"assistant "}}' '{"type":"text","sessionID":"ses_native","part":{"text":"reply"}}'
`)
	argsFile, cwdFile := filepath.Join(t.TempDir(), "args"), filepath.Join(t.TempDir(), "cwd")
	t.Setenv("OPENCODE_ARGS_FILE", argsFile)
	t.Setenv("OPENCODE_CWD_FILE", cwdFile)
	oldBinary := opencodeBinary
	t.Cleanup(func() { opencodeBinary = oldBinary })
	opencodeBinary = fake
	dir := t.TempDir()
	kb, err := spawnOpencode(SessionOpts{Model: "opencode-go/deepseek-flash", WorkDir: dir, ServerSessionID: "soul-id"})
	if err != nil {
		t.Fatal(err)
	}
	defer kb.shutdown()
	for _, turn := range []string{"soul-id-turn-1", "soul-id-turn-2"} {
		if err := kb.sendMessage("hello from test"); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(2 * time.Second)
		for _, want := range []UnifiedEventKind{UEvtTurnStarted, UEvtItemStarted, UEvtItemCompleted, UEvtTurnCompleted} {
			var ev UnifiedEvent
			select {
			case ev = <-kb.events():
			case <-deadline:
				t.Fatal("timed out waiting for opencode turn")
			}
			if ev.Kind != want || ev.TurnID != turn {
				t.Fatalf("event = %+v, want %s for %s", ev, want, turn)
			}
			if ev.Kind == UEvtItemCompleted {
				var payload UnifiedItemPayload
				if err := json.Unmarshal(ev.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if text := extractCodexResultText(payload.Result); text != "assistant reply" {
					t.Fatalf("completed text = %q", text)
				}
			}
		}
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(args), "-s\nses_native\n") != 1 || strings.Contains(string(args), "soul-id") {
		t.Fatalf("unexpected resume arguments: %s", args)
	}
	gotDir, err := os.ReadFile(cwdFile)
	wantInfo, wantErr := os.Stat(dir)
	gotInfo, gotErr := os.Stat(strings.TrimSpace(string(gotDir)))
	if err != nil || wantErr != nil || gotErr != nil || !os.SameFile(wantInfo, gotInfo) {
		t.Fatalf("working directory = %q, error = %v", gotDir, err)
	}
	if kb.info().SessionID != "soul-id" {
		t.Fatal("Soul identity changed after native session discovery")
	}
}

func TestOpencodeBackendSendMessage(t *testing.T) {
	fake := writeFakeOpencodeBinary(t, "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"text\",\"sessionID\":\"ses_async\",\"part\":{\"text\":\"reply\"}}'\n")
	kb := &opencodeBackend{bin: fake, cwd: t.TempDir(), sessionID: "soul-async", eventsCh: make(chan UnifiedEvent, opencodeEventsChanCapacity), done: make(chan struct{})}
	defer kb.shutdown()
	if err := kb.sendMessage(" "); err == nil {
		t.Fatal("expected empty message error")
	}
	if err := kb.sendMessage("hello"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-kb.events():
			if ev.Kind == UEvtTurnCompleted {
				var payload UnifiedTurnPayload
				if err := json.Unmarshal(ev.Payload, &payload); err != nil || payload.Status != "ok" || ev.TurnID != "soul-async-turn-1" {
					t.Fatalf("unexpected completion: %+v", ev)
				}
				kb.shutdown()
				if kb.alive() || kb.sendMessage("after shutdown") == nil {
					t.Fatal("shutdown backend accepted a message")
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for opencode turn")
		}
	}
}

func TestOpencodeBackendNonzeroExitIsRateLimitError(t *testing.T) {
	fake := writeFakeOpencodeBinary(t, "#!/bin/sh\necho '429 Too Many Requests' >&2\nexit 1\n")
	kb := &opencodeBackend{bin: fake, cwd: t.TempDir(), eventsCh: make(chan UnifiedEvent, opencodeEventsChanCapacity), done: make(chan struct{})}
	defer kb.shutdown()
	kb.runTurn(context.Background(), "turn-error", "hello")
	for _, want := range []UnifiedEventKind{UEvtTurnStarted, UEvtBackendError, UEvtTurnCompleted} {
		if ev := <-kb.events(); ev.Kind != want {
			t.Fatalf("event = %s, want %s", ev.Kind, want)
		}
	}
	if kb.exitCode.Load() != 1 || !kb.rateLimited.Load() {
		t.Fatal("nonzero exit was not classified as a rate-limit failure")
	}
}

func writeFakeOpencodeBinary(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}
	return path
}
