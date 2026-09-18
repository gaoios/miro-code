package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMustLock_CreatesLockFile(t *testing.T) {
	origLock := lockfile
	dir := t.TempDir()
	lockfile = filepath.Join(dir, "test.lock")
	defer func() {
		os.Remove(lockfile)
		lockfile = origLock
	}()

	// mustLock creates the file — but it also sets up signal handlers
	// and would exit if another instance is running. Test the file creation part.
	mustLock()

	data, err := os.ReadFile(lockfile)
	if err != nil {
		t.Fatalf("lock file not created: %v", err)
	}

	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatalf("lock file should contain pid, got %q", data)
	}
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want %d", pid, os.Getpid())
	}

	releaseLock()

	if _, err := os.Stat(lockfile); !os.IsNotExist(err) {
		t.Error("lock file should be removed after release")
	}
}

func TestReleaseLock_NonExistent(t *testing.T) {
	origLock := lockfile
	lockfile = filepath.Join(t.TempDir(), "nonexistent.lock")
	defer func() { lockfile = origLock }()

	// Should not panic
	releaseLock()
}

func TestMustLock_StaleLock(t *testing.T) {
	origLock := lockfile
	dir := t.TempDir()
	lockfile = filepath.Join(dir, "stale.lock")
	defer func() {
		os.Remove(lockfile)
		lockfile = origLock
	}()

	// Write a stale PID (99999999 — very unlikely to be running)
	os.WriteFile(lockfile, []byte("99999999"), 0644)

	// mustLock should detect the stale lock and replace it
	mustLock()

	data, _ := os.ReadFile(lockfile)
	pid, _ := strconv.Atoi(string(data))
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want current pid %d (stale lock not cleaned)", pid, os.Getpid())
	}

	releaseLock()
}

func TestCronTimeout(t *testing.T) {
	origMode := currentMode
	defer func() { currentMode = origMode }()

	// heartbeat always 5m
	currentMode = "heartbeat"
	if got := cronTimeout(); got != 5*time.Minute {
		t.Errorf("heartbeat timeout = %v, want 5m", got)
	}

	// cron: 10m or 20m depending on day of week (Sunday = weekly)
	currentMode = "cron"
	got := cronTimeout()
	if time.Now().Weekday() == time.Sunday {
		if got != 20*time.Minute {
			t.Errorf("cron Sunday timeout = %v, want 20m", got)
		}
	} else {
		if got != 10*time.Minute {
			t.Errorf("cron weekday timeout = %v, want 10m", got)
		}
	}

	// default: 30m
	currentMode = "interactive"
	if got := cronTimeout(); got != 30*time.Minute {
		t.Errorf("interactive timeout = %v, want 30m", got)
	}
}

func TestGetModelEndpoint(t *testing.T) {
	// Isolate all config search paths
	dir := t.TempDir()
	origAppHome, origAppDir, origWS := appHome, appDir, workspace
	appHome = dir
	appDir = filepath.Join(dir, "data")
	workspace = filepath.Join(dir, "ws")
	os.MkdirAll(appDir, 0755)
	defer func() { appHome = origAppHome; appDir = origAppDir; workspace = origWS }()

	cfg := map[string]interface{}{
		"providers": map[string]interface{}{
			"minimax": map[string]interface{}{
				"baseUrl": "https://api.minimax.chat/v1",
				"type":    "openai",
			},
		},
	}
	data, _ := json.Marshal(cfg)
	os.WriteFile(filepath.Join(appDir, "config.json"), data, 0644)

	got := getModelEndpoint()
	want := "https://api.minimax.chat/v1/models"
	if got != want {
		t.Errorf("getModelEndpoint() = %q, want %q", got, want)
	}
}

func TestGetModelEndpoint_NoCfg(t *testing.T) {
	dir := t.TempDir()
	origAppHome, origAppDir, origWS := appHome, appDir, workspace
	appHome = dir
	appDir = filepath.Join(dir, "data")
	workspace = filepath.Join(dir, "ws")
	defer func() { appHome = origAppHome; appDir = origAppDir; workspace = origWS }()

	got := getModelEndpoint()
	if got != "" {
		t.Errorf("getModelEndpoint() = %q, want empty", got)
	}
}

func TestPreflight_ClaudeMissing(t *testing.T) {
	origBin := claudeBin
	claudeBin = "/nonexistent/claude"
	defer func() { claudeBin = origBin }()

	warnings := preflight()
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "claude") && strings.Contains(w, "not found") {
			found = true
		}
	}
	if !found {
		t.Errorf("preflight should warn about missing claude binary, got: %v", warnings)
	}
}

func TestRotateMetrics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.jsonl")

	// Write 15 lines
	var content string
	for i := 0; i < 15; i++ {
		content += `{"mode":"test","exit_code":0}` + "\n"
	}
	os.WriteFile(path, []byte(content), 0644)

	// Rotate with max=10, keep=5
	rotateMetrics(path, 10, 5)

	data, _ := os.ReadFile(path)
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != 5 {
		t.Errorf("after rotation got %d lines, want 5", lines)
	}
}

func TestMustLock_BadPidInLockFile(t *testing.T) {
	origLock := lockfile
	dir := t.TempDir()
	lockfile = filepath.Join(dir, "bad.lock")
	defer func() {
		os.Remove(lockfile)
		lockfile = origLock
	}()

	// Write non-numeric content
	os.WriteFile(lockfile, []byte("not-a-pid"), 0644)

	// mustLock should handle this gracefully
	mustLock()

	data, _ := os.ReadFile(lockfile)
	pid, _ := strconv.Atoi(string(data))
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want current pid %d", pid, os.Getpid())
	}

	releaseLock()
}

// ── Crash Recovery Tests ──

func TestGetLastCrashInfo_NoCrash(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "data", "sessions.db")
	os.MkdirAll(filepath.Dir(dbPath), 0755)
	defer func() { dbPath = origDB }()

	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	os.WriteFile(metricsPath, []byte(`{"ts":"`+time.Now().Format(time.RFC3339)+`","mode":"heartbeat","exit_code":0,"duration_s":30}`+"\n"), 0644)

	if crash := getLastCrashInfo("heartbeat"); crash != nil {
		t.Errorf("expected nil for successful last run, got %+v", crash)
	}
}

func TestGetLastCrashInfo_WithCrash(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "data", "sessions.db")
	os.MkdirAll(filepath.Dir(dbPath), 0755)
	defer func() { dbPath = origDB }()

	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	ts := time.Now().Add(-5 * time.Minute).Format(time.RFC3339)
	os.WriteFile(metricsPath, []byte(`{"ts":"`+ts+`","mode":"heartbeat","exit_code":1,"session_id":"abc123","error":"timeout after 300s"}`+"\n"), 0644)

	crash := getLastCrashInfo("heartbeat")
	if crash == nil {
		t.Fatal("expected crash info, got nil")
	}
	if crash.ExitCode != 1 {
		t.Errorf("exit_code = %d, want 1", crash.ExitCode)
	}
	if crash.SessionID != "abc123" {
		t.Errorf("session_id = %q, want %q", crash.SessionID, "abc123")
	}
	if crash.Error != "timeout after 300s" {
		t.Errorf("error = %q, want %q", crash.Error, "timeout after 300s")
	}
}

func TestGetLastCrashInfo_ExpiredCrash(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "data", "sessions.db")
	os.MkdirAll(filepath.Dir(dbPath), 0755)
	defer func() { dbPath = origDB }()

	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	ts := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	os.WriteFile(metricsPath, []byte(`{"ts":"`+ts+`","mode":"heartbeat","exit_code":1,"error":"old crash"}`+"\n"), 0644)

	if crash := getLastCrashInfo("heartbeat"); crash != nil {
		t.Errorf("expected nil for expired crash (>1h), got %+v", crash)
	}
}

func TestGetLastCrashInfo_MixedModes(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "data", "sessions.db")
	os.MkdirAll(filepath.Dir(dbPath), 0755)
	defer func() { dbPath = origDB }()

	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	ts := time.Now().Add(-5 * time.Minute).Format(time.RFC3339)
	content := `{"ts":"` + ts + `","mode":"cron","exit_code":1,"error":"cron fail"}` + "\n" +
		`{"ts":"` + ts + `","mode":"heartbeat","exit_code":0}` + "\n"
	os.WriteFile(metricsPath, []byte(content), 0644)

	// heartbeat was successful (last record)
	if crash := getLastCrashInfo("heartbeat"); crash != nil {
		t.Errorf("heartbeat should be nil (last was success), got %+v", crash)
	}

	// cron crashed (only record for that mode)
	crash := getLastCrashInfo("cron")
	if crash == nil {
		t.Fatal("cron should have crash info")
	}
	if crash.ExitCode != 1 {
		t.Errorf("cron exit_code = %d, want 1", crash.ExitCode)
	}
}

func TestGetLastCrashInfo_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "data", "sessions.db")
	os.MkdirAll(filepath.Dir(dbPath), 0755)
	defer func() { dbPath = origDB }()

	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	os.WriteFile(metricsPath, []byte(""), 0644)

	if crash := getLastCrashInfo("heartbeat"); crash != nil {
		t.Errorf("expected nil for empty file, got %+v", crash)
	}
}

func TestGetLastCrashInfo_NoFile(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "data", "sessions.db")
	os.MkdirAll(filepath.Dir(dbPath), 0755)
	defer func() { dbPath = origDB }()

	if crash := getLastCrashInfo("heartbeat"); crash != nil {
		t.Errorf("expected nil for missing file, got %+v", crash)
	}
}

func TestBuildEventDigest(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "data", "sessions.db")
	os.MkdirAll(filepath.Dir(dbPath), 0755)
	defer func() { dbPath = origDB }()

	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	ts := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)

	content := `{"ts":"` + ts + `","mode":"heartbeat","exit_code":0,"event_type":"success"}` + "\n" +
		`{"ts":"` + ts + `","mode":"cron","exit_code":1,"event_type":"crash","error":"some weird new error"}` + "\n" +
		`{"ts":"` + ts + `","mode":"cron","exit_code":1,"event_type":"timeout","error":"timeout after 5m0s"}` + "\n"
	os.WriteFile(metricsPath, []byte(content), 0644)

	digest := buildEventDigest(24 * time.Hour)
	if digest == "" {
		t.Fatal("expected non-empty digest")
	}
	if !strings.Contains(digest, "3 runs") {
		t.Errorf("digest should show 3 runs, got: %s", digest)
	}
	if !strings.Contains(digest, "2 failures") {
		t.Errorf("digest should show 2 failures, got: %s", digest)
	}
	if !strings.Contains(digest, "Unclassified crashes") {
		t.Errorf("digest should highlight unclassified crashes, got: %s", digest)
	}
	if !strings.Contains(digest, "some weird new error") {
		t.Errorf("digest should include the unclassified error text, got: %s", digest)
	}
}

func TestBuildEventDigest_Empty(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "data", "sessions.db")
	os.MkdirAll(filepath.Dir(dbPath), 0755)
	defer func() { dbPath = origDB }()

	// No metrics file
	if digest := buildEventDigest(24 * time.Hour); digest != "" {
		t.Errorf("expected empty digest for missing metrics, got: %s", digest)
	}
}

func TestResolveFuzzyModel(t *testing.T) {
	// Set up a temp config.json with test providers
	dir := t.TempDir()
	cfg := `{
		"providers": {
			"minimax": {
				"baseUrl": "https://api.minimaxi.com/anthropic",
				"apiKey": "test-key",
				"models": ["MiniMax-M2.7-highspeed"]
			},
			"zai": {
				"baseUrl": "https://api.z.ai/api/anthropic",
				"models": ["glm-5.1"]
			}
		}
	}`
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "config.json"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}

	// Override appHome so loadAllProviders finds our test config
	origAppHome := appHome
	appHome = dir
	defer func() { appHome = origAppHome }()

	tests := []struct {
		input    string
		want     string
		wantErr  bool
	}{
		// Native aliases
		{"opus", "claude-opus-4-7", false},
		{"haiku", "claude-haiku-4-5-20251001", false},
		{"sonnet", "claude-sonnet-4-6", false},

		// Exact provider/model
		{"zai/glm-5.1", "zai/glm-5.1", false},
		{"minimax/MiniMax-M2.7-highspeed", "minimax/MiniMax-M2.7-highspeed", false},

		// Provider-only -> first model
		{"minimax", "minimax/MiniMax-M2.7-highspeed", false},
		{"zai", "zai/glm-5.1", false},

		// Fuzzy substring
		{"highspeed", "minimax/MiniMax-M2.7-highspeed", false},
		{"glm", "zai/glm-5.1", false},
		{"M2.7", "minimax/MiniMax-M2.7-highspeed", false},
		{"m2.7", "minimax/MiniMax-M2.7-highspeed", false},

		// No match
		{"nonexistent", "", true},

		// Empty -> empty
		{"", "", false},

		// Provider validation: known provider/model passes through
		{"zai/glm-5.1", "zai/glm-5.1", false},
		// Provider validation: unknown provider fails (silent pass-through bug fix 2026-05-04)
		{"openai/glm-5.1", "", true},
		{"anthropic/glm-5.1", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := resolveFuzzyModel(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("resolveFuzzyModel(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("resolveFuzzyModel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestClassifyExitEvent(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int
		errMsg   string
		stderr   string
		want     string
	}{
		{"success", 0, "", "", "success"},
		{"timeout_from_err", 1, "timeout after 5m0s", "", "timeout"},
		{"timeout_from_stderr", 1, "", "claude timed out", "timeout"},
		{"login_expired", 1, "", "Error: not logged in. Please run claude login", "login_expired"},
		{"oauth_issue", 1, "", "OAuth token expired", "login_expired"},
		{"context_overflow", 1, "", "prompt is too long for context window", "context_overflow"},
		{"rate_limit_429", 1, "", "HTTP 429 too many requests", "rate_limit"},
		{"rate_limit_quota", 1, "", "quota exceeded for model", "rate_limit"},
		{"network_refused", 1, "", "connection refused", "network_error"},
		{"api_500", 1, "", "500 Internal Server Error", "api_error"},
		{"api_502", 1, "", "502 Bad Gateway", "api_error"},
		{"interrupted", 130, "", "", "interrupted"},
		{"aborted", 134, "", "", "aborted"},
		{"oom_killed", 137, "", "", "oom_killed"},
		{"segfault", 139, "", "", "segfault"},
		{"terminated", 143, "", "", "terminated"},
		{"generic_crash", 1, "", "", "crash"},
		{"generic_nonzero", 2, "", "some unknown error", "crash"},
		{"entrypoint_issue", 1, "", "CLAUDE_CODE_ENTRYPOINT not set", "login_expired"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyExitEvent(tt.exitCode, tt.errMsg, tt.stderr)
			if got != tt.want {
				t.Errorf("classifyExitEvent(%d, %q, %q) = %q, want %q",
					tt.exitCode, tt.errMsg, tt.stderr, got, tt.want)
			}
		})
	}
}

func TestLimitedBuffer(t *testing.T) {
	var lb limitedBuffer
	lb.limit = 10

	lb.Write([]byte("hello"))
	if lb.String() != "hello" {
		t.Errorf("got %q, want %q", lb.String(), "hello")
	}

	lb.Write([]byte(" world and more"))
	got := lb.String()
	if len(got) != 10 {
		t.Errorf("len = %d, want 10", len(got))
	}
	// Should keep the last 10 bytes: "d and more"
	if got != "d and more" {
		t.Errorf("got %q, want %q", got, "d and more")
	}
}

func TestLimitedBuffer_ZeroLimit(t *testing.T) {
	var lb limitedBuffer
	// limit=0 means unlimited
	lb.Write([]byte("all the data"))
	if lb.String() != "all the data" {
		t.Errorf("got %q", lb.String())
	}
}