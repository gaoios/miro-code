package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgyBackendSendMessageEmitsUnifiedEvents(t *testing.T) {
	fakeAgy := filepath.Join(t.TempDir(), "agy")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" > "$AGY_ARGS_FILE"
case " $* " in *" --print hello from test "*) ;; *) exit 21 ;; esac
case " $* " in *" --model gemini-3.1-pro-low "*) ;; *) exit 22 ;; esac
case " $* " in *" --effort low "*) ;; *) exit 23 ;; esac
case " $* " in *" --mode accept-edits "*) ;; *) exit 24 ;; esac
case " $* " in *" --dangerously-skip-permissions "*) ;; *) exit 25 ;; esac
printf 'assistant reply\n'
`
	if err := os.WriteFile(fakeAgy, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("AGY_ARGS_FILE", argsFile)

	oldBinary, oldModelMap := agyBinary, agyModelMap
	oldMode, oldEffort, oldSkip := agyMode, agyEffort, agyDangerouslySkipPermissions
	t.Cleanup(func() {
		agyBinary, agyModelMap = oldBinary, oldModelMap
		agyMode, agyEffort, agyDangerouslySkipPermissions = oldMode, oldEffort, oldSkip
	})
	agyBinary = fakeAgy
	agyModelMap = map[string]string{"alias": "gemini-3.1-pro-low"}
	agyMode = "accept-edits"
	agyEffort = "low"
	agyDangerouslySkipPermissions = true

	ab, err := spawnAgy(SessionOpts{Model: "alias", WorkDir: t.TempDir(), ServerSessionID: "server-session-1"})
	if err != nil {
		t.Fatalf("spawnAgy returned error: %v", err)
	}
	defer ab.shutdown()
	if info := ab.info(); info.Kind != BackendAgy || info.Model != "gemini-3.1-pro-low" {
		t.Fatalf("bad backend info: %+v", info)
	}
	if err := ab.sendMessage("hello from test"); err != nil {
		t.Fatalf("sendMessage returned error: %v", err)
	}

	var completedText string
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ab.events():
			if ev.Kind == UEvtItemCompleted {
				var payload UnifiedItemPayload
				if err := json.Unmarshal(ev.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				completedText = extractCodexResultText(payload.Result)
			}
			if ev.Kind == UEvtTurnCompleted {
				if completedText != "assistant reply" {
					t.Fatalf("completed text = %q", completedText)
				}
				if raw, err := os.ReadFile(argsFile); err != nil || len(raw) == 0 {
					t.Fatalf("fake agy args not recorded: %v", err)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for Antigravity turn completion")
		}
	}
}

func TestAgyCommandArgsDefaultToPlanWithoutDangerousPermissions(t *testing.T) {
	oldMode, oldEffort, oldSkip := agyMode, agyEffort, agyDangerouslySkipPermissions
	t.Cleanup(func() { agyMode, agyEffort, agyDangerouslySkipPermissions = oldMode, oldEffort, oldSkip })
	agyMode, agyEffort, agyDangerouslySkipPermissions = "plan", "low", false

	ab := &agyBackend{model: "gemini-3.1-pro-low", mode: agyMode, effort: agyEffort, dangerouslySkipPermissions: agyDangerouslySkipPermissions}
	args := strings.Join(ab.commandArgs("inspect only"), " ")
	if !strings.Contains(args, "--mode plan") {
		t.Fatalf("args missing safe plan mode: %s", args)
	}
	if strings.Contains(args, "--dangerously-skip-permissions") {
		t.Fatalf("safe defaults include dangerous permissions: %s", args)
	}
}

func TestParseAgyTextOutput(t *testing.T) {
	_, err := parseAgyTextOutput([]byte("   "))
	if err == nil {
		t.Fatal("expected error for empty text, got nil")
	}
	text, err := parseAgyTextOutput([]byte(" hello world \n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "hello world" {
		t.Fatalf("expected 'hello world', got %q", text)
	}
}

func TestAgyBackendRunTurnHandlesErrorsAndRateLimit(t *testing.T) {
	fakeAgy := filepath.Join(t.TempDir(), "agy")
	script := `#!/bin/sh
echo "Rate limit exceeded. Please try again later." >&2
exit 1
`
	if err := os.WriteFile(fakeAgy, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	oldBinary := agyBinary
	t.Cleanup(func() { agyBinary = oldBinary })
	agyBinary = fakeAgy

	ab, err := spawnAgy(SessionOpts{WorkDir: t.TempDir(), ServerSessionID: "server-session-err"})
	if err != nil {
		t.Fatalf("spawnAgy returned error: %v", err)
	}
	defer ab.shutdown()

	if err := ab.sendMessage("trigger error"); err != nil {
		t.Fatalf("sendMessage returned error: %v", err)
	}

	deadline := time.After(2 * time.Second)
	var observedError bool
	for {
		select {
		case ev := <-ab.events():
			if ev.Kind == UEvtTurnCompleted {
				var payload UnifiedTurnPayload
				if err := json.Unmarshal(ev.Payload, &payload); err == nil {
					if payload.Status == "error" && strings.Contains(payload.Error, "Rate limit exceeded") {
						observedError = true
					}
				}
				if !observedError {
					t.Fatalf("expected error turn completion, got: %s", string(ev.Payload))
				}
				if !ab.rateLimited.Load() {
					t.Fatalf("expected rateLimited to be marked true")
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for error turn completion")
		}
	}
}
