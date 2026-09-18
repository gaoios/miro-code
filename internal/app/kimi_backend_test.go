package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseKimiTextOutputRejectsMissingLLM(t *testing.T) {
	if _, err := parseKimiTextOutput([]byte("LLM not set\n")); err == nil {
		t.Fatal("expected LLM not set to be treated as a backend error")
	}
}

func TestParseKimiTextOutputAcceptsAssistantText(t *testing.T) {
	text, err := parseKimiTextOutput([]byte(" assistant reply \n"))
	if err != nil {
		t.Fatalf("parseKimiTextOutput returned error: %v", err)
	}
	if text != "assistant reply" {
		t.Fatalf("text = %q, want assistant reply", text)
	}
}

func TestKimiBackendSendMessageEmitsUnifiedEvents(t *testing.T) {
	fakeKimi := writeFakeKimiBinary(t, `#!/bin/sh
set -eu
printf '%s\n' "$*" > "$KIMI_ARGS_FILE"
case " $* " in *" --print "*) ;; *) exit 21 ;; esac
case " $* " in *" --output-format text "*) ;; *) exit 22 ;; esac
case " $* " in *" --final-message-only "*) ;; *) exit 23 ;; esac
case " $* " in *" --work-dir "*) ;; *) exit 24 ;; esac
case " $* " in *" --prompt hello from test "*) ;; *) exit 25 ;; esac
case " $* " in *" --model kimi-fake "*) ;; *) exit 26 ;; esac
printf 'assistant reply\n'
`)
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("KIMI_ARGS_FILE", argsFile)

	oldBinary := kimiBinary
	oldModelMap := kimiModelMap
	t.Cleanup(func() {
		kimiBinary = oldBinary
		kimiModelMap = oldModelMap
	})
	kimiBinary = fakeKimi
	kimiModelMap = map[string]string{"alias": "kimi-fake"}

	kb, err := spawnKimi(SessionOpts{Model: "alias", WorkDir: t.TempDir(), ServerSessionID: "server-session-1"})
	if err != nil {
		t.Fatalf("spawnKimi returned error: %v", err)
	}
	defer kb.shutdown()

	if info := kb.info(); info.Kind != BackendKimi || info.Model != "kimi-fake" || info.SessionID == "" {
		t.Fatalf("bad backend info: %+v", info)
	}
	if err := kb.sendMessage("hello from test"); err != nil {
		t.Fatalf("sendMessage returned error: %v", err)
	}

	seen := map[UnifiedEventKind]bool{}
	var completedText string
	deadline := time.After(2 * time.Second)
	for !seen[UEvtTurnCompleted] {
		select {
		case ev := <-kb.events():
			seen[ev.Kind] = true
			if ev.Kind == UEvtItemCompleted {
				var payload UnifiedItemPayload
				if err := json.Unmarshal(ev.Payload, &payload); err != nil {
					t.Fatalf("bad item payload: %v", err)
				}
				completedText = extractCodexResultText(payload.Result)
			}
		case <-deadline:
			t.Fatal("timed out waiting for Kimi turn completion")
		}
	}
	if completedText != "assistant reply" {
		t.Fatalf("completed text = %q, want assistant reply", completedText)
	}
	if raw, err := os.ReadFile(argsFile); err != nil || len(raw) == 0 {
		t.Fatalf("fake Kimi args not recorded: %v", err)
	}
}

func writeFakeKimiBinary(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kimi")
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatalf("write fake kimi: %v", err)
	}
	return path
}
