package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseGrokHeadlessOutput(t *testing.T) {
	text, err := parseGrokHeadlessOutput([]byte(`{"text":"GROK_OK","sessionId":"grok-session-1","stopReason":"end_turn"}`))
	if err != nil {
		t.Fatalf("parseGrokHeadlessOutput returned error: %v", err)
	}
	if text != "GROK_OK" {
		t.Fatalf("text = %q, want GROK_OK", text)
	}
}

func TestParseGrokHeadlessOutputRejectsMissingText(t *testing.T) {
	if _, err := parseGrokHeadlessOutput([]byte(`{"sessionId":"grok-session-1"}`)); err == nil {
		t.Fatal("expected missing text error")
	}
}

func TestGrokBackendSendMessageEmitsUnifiedEvents(t *testing.T) {
	fakeGrok := writeFakeGrokBinary(t, `#!/bin/sh
set -eu
printf '%s\n' "$*" > "$GROK_ARGS_FILE"
case " $* " in
  *" -p hello from test "* ) ;;
  *) echo "missing prompt arg: $*" >&2; exit 23 ;;
esac
case " $* " in
  *" --cwd "* ) ;;
  *) echo "missing cwd arg: $*" >&2; exit 24 ;;
esac
case " $* " in
  *" --output-format json "* ) ;;
  *) echo "missing json output arg: $*" >&2; exit 25 ;;
esac
case " $* " in
  *" --max-turns 1 "* ) ;;
  *) echo "missing max turns arg: $*" >&2; exit 26 ;;
esac
case " $* " in
  *" --no-subagents "* ) ;;
  *) echo "missing no-subagents arg: $*" >&2; exit 27 ;;
esac
case " $* " in
  *" --disable-web-search "* ) ;;
  *) echo "missing disable-web-search arg: $*" >&2; exit 28 ;;
esac
case " $* " in
  *" --no-memory "* ) ;;
  *) echo "missing no-memory arg: $*" >&2; exit 29 ;;
esac
printf '{"text":"assistant reply","sessionId":"grok-session-1","stopReason":"end_turn"}\n'
`)
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("GROK_ARGS_FILE", argsFile)

	oldBinary := grokBinary
	oldModelMap := grokModelMap
	t.Cleanup(func() {
		grokBinary = oldBinary
		grokModelMap = oldModelMap
	})
	grokBinary = fakeGrok
	grokModelMap = map[string]string{"alias": "grok-fake"}

	gb, err := spawnGrok(SessionOpts{
		Model:           "alias",
		WorkDir:         t.TempDir(),
		ServerSessionID: "server-session-1",
	})
	if err != nil {
		t.Fatalf("spawnGrok returned error: %v", err)
	}
	defer gb.shutdown()

	if !gb.waitInit(10 * time.Millisecond) {
		t.Fatal("waitInit returned false")
	}
	if info := gb.info(); info.Kind != BackendGrok || info.Model != "grok-fake" || info.SessionID == "" {
		t.Fatalf("bad backend info: %+v", info)
	}
	if err := gb.sendMessage("hello from test"); err != nil {
		t.Fatalf("sendMessage returned error: %v", err)
	}

	seen := map[UnifiedEventKind]bool{}
	var completedText string
	deadline := time.After(2 * time.Second)
	for !seen[UEvtTurnCompleted] {
		select {
		case ev := <-gb.events():
			seen[ev.Kind] = true
			if ev.Kind == UEvtItemCompleted {
				if ev.ItemKind != UItemAgentMessage {
					t.Fatalf("completed item kind = %q, want agent_message", ev.ItemKind)
				}
				var payload UnifiedItemPayload
				if err := json.Unmarshal(ev.Payload, &payload); err != nil {
					t.Fatalf("bad item payload: %v", err)
				}
				completedText = extractCodexResultText(payload.Result)
			}
		case <-deadline:
			t.Fatal("timed out waiting for Grok turn completion")
		}
	}

	for _, want := range []UnifiedEventKind{UEvtTurnStarted, UEvtItemStarted, UEvtItemCompleted, UEvtTurnCompleted} {
		if !seen[want] {
			t.Fatalf("missing event kind %q; seen=%v", want, seen)
		}
	}
	if completedText != "assistant reply" {
		t.Fatalf("completed text = %q, want assistant reply", completedText)
	}

	argsRaw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading fake grok args: %v", err)
	}
	if string(argsRaw) == "" {
		t.Fatal("fake grok args file is empty")
	}
}

func TestGrokBackendSendMessageReportsCommandError(t *testing.T) {
	fakeGrok := writeFakeGrokBinary(t, `#!/bin/sh
echo "grok failed intentionally" >&2
exit 42
`)
	oldBinary := grokBinary
	t.Cleanup(func() { grokBinary = oldBinary })
	grokBinary = fakeGrok

	gb, err := spawnGrok(SessionOpts{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("spawnGrok returned error: %v", err)
	}
	defer gb.shutdown()

	if err := gb.sendMessage("fail please"); err != nil {
		t.Fatalf("sendMessage returned error: %v", err)
	}

	sawBackendError := false
	sawTurnError := false
	deadline := time.After(2 * time.Second)
	for !(sawBackendError && sawTurnError) {
		select {
		case ev := <-gb.events():
			if ev.Kind == UEvtBackendError {
				sawBackendError = true
			}
			if ev.Kind == UEvtTurnCompleted {
				var payload UnifiedTurnPayload
				_ = json.Unmarshal(ev.Payload, &payload)
				if payload.Status == "error" && payload.Error != "" {
					sawTurnError = true
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for error events; backend=%v turn=%v", sawBackendError, sawTurnError)
		}
	}
}

func TestCreateSessionWithOptsGrokBypassesFuzzyModelResolver(t *testing.T) {
	fakeGrok := writeFakeGrokBinary(t, `#!/bin/sh
printf '{"text":"unused"}\n'
`)
	oldBinary := grokBinary
	oldWorkspace := workspace
	oldAppDir := appDir
	oldDBPath := dbPath
	t.Cleanup(func() {
		grokBinary = oldBinary
		workspace = oldWorkspace
		appDir = oldAppDir
		dbPath = oldDBPath
	})
	grokBinary = fakeGrok
	workspace = t.TempDir()
	appDir = t.TempDir()
	dbPath = filepath.Join(appDir, "sessions.db")
	project := t.TempDir()

	sm := newSessionManager(5, time.Hour, time.Hour)
	defer sm.shutdownAll()

	sess, err := sm.createSessionWithOpts(sessionCreateOpts{
		Name:     "grok-native-model",
		Project:  project,
		Model:    "grok-native-model",
		Soul:     false,
		Backend:  BackendGrok,
		Category: CategoryInteractive,
	})
	if err != nil {
		t.Fatalf("createSessionWithOpts returned error: %v", err)
	}
	if sess.Backend != BackendGrok {
		t.Fatalf("session backend = %q, want grok", sess.Backend)
	}
	if sess.Model != "grok-native-model" {
		t.Fatalf("session model = %q, want grok-native-model", sess.Model)
	}
	info := sess.process.info()
	if info.Kind != BackendGrok || info.Model != "grok-native-model" {
		t.Fatalf("backend info = %+v, want grok/native model", info)
	}
}

func writeFakeGrokBinary(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grok")
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatalf("write fake grok: %v", err)
	}
	return path
}
