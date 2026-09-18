package app

import "testing"

func TestParseArgs_Interactive(t *testing.T) {
	mode, prompt, extra := parseArgs(nil)
	if mode != "interactive" {
		t.Errorf("mode = %q, want interactive", mode)
	}
	if prompt != "" {
		t.Errorf("prompt = %q, want empty", prompt)
	}
	if len(extra) != 0 {
		t.Errorf("extra = %v, want empty", extra)
	}
}

func TestParseArgs_Print(t *testing.T) {
	mode, prompt, extra := parseArgs([]string{"-p", "hello world"})
	if mode != "print" {
		t.Errorf("mode = %q, want print", mode)
	}
	if prompt != "hello world" {
		t.Errorf("prompt = %q, want 'hello world'", prompt)
	}
	if len(extra) != 0 {
		t.Errorf("extra = %v, want empty", extra)
	}
}

func TestParseArgs_PrintWithExtra(t *testing.T) {
	overrideModel = "" // reset global
	mode, prompt, extra := parseArgs([]string{"-p", "task", "--model", "haiku"})
	if mode != "print" {
		t.Errorf("mode = %q, want print", mode)
	}
	if prompt != "task" {
		t.Errorf("prompt = %q, want 'task'", prompt)
	}
	// --model is consumed by parseArgs (sets overrideModel), not passed as extra
	if len(extra) != 0 {
		t.Errorf("extra = %v, want []", extra)
	}
	// resolveFuzzyModel expands "haiku" to its full native model ID
	if overrideModel != "claude-haiku-4-5-20251001" {
		t.Errorf("overrideModel = %q, want 'claude-haiku-4-5-20251001'", overrideModel)
	}
	overrideModel = "" // cleanup
}

func TestParseArgs_Category(t *testing.T) {
	overrideCategory = "" // reset global
	mode, prompt, extra := parseArgs([]string{"--category", "cron", "-p", "do stuff"})
	if mode != "print" {
		t.Errorf("mode = %q, want print", mode)
	}
	if prompt != "do stuff" {
		t.Errorf("prompt = %q, want 'do stuff'", prompt)
	}
	if overrideCategory != "cron" {
		t.Errorf("overrideCategory = %q, want 'cron'", overrideCategory)
	}
	if len(extra) != 0 {
		t.Errorf("extra = %v, want []", extra)
	}
	overrideCategory = "" // cleanup
}

func TestParseArgs_Cron(t *testing.T) {
	mode, _, _ := parseArgs([]string{"--cron"})
	if mode != "cron" {
		t.Errorf("mode = %q, want cron", mode)
	}
}

func TestParseArgs_Heartbeat(t *testing.T) {
	mode, _, _ := parseArgs([]string{"--heartbeat"})
	if mode != "heartbeat" {
		t.Errorf("mode = %q, want heartbeat", mode)
	}
}

func TestParseArgs_DB(t *testing.T) {
	mode, _, extra := parseArgs([]string{"db", "stats"})
	if mode != "db" {
		t.Errorf("mode = %q, want db", mode)
	}
	if len(extra) != 1 || extra[0] != "stats" {
		t.Errorf("extra = %v, want [stats]", extra)
	}
}

func TestParseArgs_Sessions(t *testing.T) {
	mode, _, extra := parseArgs([]string{"sessions", "nginx"})
	if mode != "sessions" {
		t.Errorf("mode = %q, want sessions", mode)
	}
	if len(extra) != 1 || extra[0] != "nginx" {
		t.Errorf("extra = %v, want [nginx]", extra)
	}
}

func TestParseArgs_SessionsShort(t *testing.T) {
	mode, _, extra := parseArgs([]string{"ss", "gallery"})
	if mode != "sessions" {
		t.Errorf("mode = %q, want sessions", mode)
	}
	if len(extra) != 1 || extra[0] != "gallery" {
		t.Errorf("extra = %v, want [gallery]", extra)
	}
}

func TestParseArgs_Notify(t *testing.T) {
	mode, _, extra := parseArgs([]string{"notify", "hello", "world"})
	if mode != "notify" {
		t.Errorf("mode = %q, want notify", mode)
	}
	if len(extra) != 2 || extra[0] != "hello" {
		t.Errorf("extra = %v, want [hello world]", extra)
	}
}

func TestParseArgs_NotifyPhoto(t *testing.T) {
	mode, _, extra := parseArgs([]string{"notify-photo", "https://example.com/img.jpg", "caption"})
	if mode != "notify-photo" {
		t.Errorf("mode = %q, want notify-photo", mode)
	}
	if len(extra) != 2 {
		t.Errorf("extra = %v, want 2 items", extra)
	}
}

func TestParseArgs_Status(t *testing.T) {
	mode, _, _ := parseArgs([]string{"status"})
	if mode != "status" {
		t.Errorf("mode = %q, want status", mode)
	}
}

func TestParseArgs_Log(t *testing.T) {
	mode, _, extra := parseArgs([]string{"log", "2"})
	if mode != "log" {
		t.Errorf("mode = %q, want log", mode)
	}
	if len(extra) != 1 || extra[0] != "2" {
		t.Errorf("extra = %v, want [2]", extra)
	}
}

func TestParseArgs_Build(t *testing.T) {
	mode, _, _ := parseArgs([]string{"build"})
	if mode != "build" {
		t.Errorf("mode = %q, want build", mode)
	}
}

func TestParseArgs_Versions(t *testing.T) {
	mode, _, _ := parseArgs([]string{"versions"})
	if mode != "versions" {
		t.Errorf("mode = %q, want versions", mode)
	}
}

func TestParseArgs_Rollback(t *testing.T) {
	mode, _, extra := parseArgs([]string{"rollback", "2"})
	if mode != "rollback" {
		t.Errorf("mode = %q, want rollback", mode)
	}
	if len(extra) != 1 || extra[0] != "2" {
		t.Errorf("extra = %v, want [2]", extra)
	}
}

func TestParseArgs_Update(t *testing.T) {
	mode, _, _ := parseArgs([]string{"update"})
	if mode != "update" {
		t.Errorf("mode = %q, want update", mode)
	}
}

func TestParseArgs_Config(t *testing.T) {
	mode, _, _ := parseArgs([]string{"config"})
	if mode != "config" {
		t.Errorf("mode = %q, want config", mode)
	}
}

func TestParseArgs_Clean(t *testing.T) {
	mode, _, _ := parseArgs([]string{"clean"})
	if mode != "clean" {
		t.Errorf("mode = %q, want clean", mode)
	}
}

func TestParseArgs_InteractiveWithResume(t *testing.T) {
	mode, _, extra := parseArgs([]string{"-c"})
	if mode != "interactive" {
		t.Errorf("mode = %q, want interactive", mode)
	}
	if len(extra) != 1 || extra[0] != "-c" {
		t.Errorf("extra = %v, want [-c]", extra)
	}
}

func TestParseArgs_Doctor(t *testing.T) {
	mode, _, _ := parseArgs([]string{"doctor"})
	if mode != "doctor" {
		t.Errorf("mode = %q, want doctor", mode)
	}
}

func TestParseArgs_Diff(t *testing.T) {
	mode, _, _ := parseArgs([]string{"diff"})
	if mode != "diff" {
		t.Errorf("mode = %q, want diff", mode)
	}
}

func TestParseArgs_New(t *testing.T) {
	mode, _, _ := parseArgs([]string{"new"})
	if mode != "new" {
		t.Errorf("mode = %q, want new", mode)
	}
}

func TestParseArgs_PrintMissingPrompt(t *testing.T) {
	mode, prompt, _ := parseArgs([]string{"-p"})
	if mode != "print" {
		t.Errorf("mode = %q, want print", mode)
	}
	if prompt != "" {
		t.Errorf("prompt = %q, want empty (no arg after -p)", prompt)
	}
}
