// webhook.go — Skill-driven webhook framework.
//
// Concept: a single POST /webhook endpoint receives JSON bodies, routes
// by `action` field to a plugin file under workspace/webhook/, and
// falls back to an AI-spawned session on plugin miss/failure. Plugins
// are arbitrary executables (.sh / .py / .go / .js / .ts / chmod+x
// binaries) that read the raw JSON on stdin and emit a result JSON on
// stdout.
//
// On failure (exit != 0, exit == 64 explicit fallback signal, timeout,
// or missing plugin), the framework spawns a session that's instructed
// to (1) complete the task by any means and (2) repair the plugin.
// Over time the framework grows new plugins automatically.
//
// ── Framework vs user data ─────────────────────────────────────────────
//
// This file is part of soul-cli (open-source). It does NOT know what
// any individual webhook does — no host aliases, no download paths, no
// output directory conventions. Anything that's specific to one user's
// setup (where to put downloaded files, how to map URL hosts to plugin
// names, what an "action" semantically means) lives in user-side
// plugins under `workspace/webhook/`.
//
// The framework only handles:
//   - dispatch (action → plugin file)
//   - exec (stdin/stdout JSON, timeouts, env, audit)
//   - fallback (spawn AI session on failure)
//
// Everything else is the plugin's job.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ── Constants ──────────────────────────────────────────────────────────────

const (
	webhookDirName          = "webhook"        // workspace subdir for plugins
	webhookBinSubdir        = "_bin"           // compiled .go plugin cache
	webhookDefaultTimeout   = 30 * time.Second // override via body.timeout
	webhookMaxTimeout       = 5 * time.Minute  // hard cap
	webhookMaxStdoutBytes   = 10 * 1024 * 1024 // 10MB
	webhookMaxBodyBytes     = 1 << 20          // 1MB inbound
	webhookFallbackExitCode = 64               // plugin signals "I don't handle this"
	webhookAuditFieldMax    = 8 * 1024         // truncate stdout/stderr for audit row
)

// errPluginNotFound: plugin file does not exist for the requested action.
// Triggers AI fallback rather than a hard error.
var errPluginNotFound = errors.New("plugin not found")

// ── Path helpers ───────────────────────────────────────────────────────────

func webhookDir() string {
	return filepath.Join(workspace, webhookDirName)
}

func webhookBinDir() string {
	return filepath.Join(webhookDir(), webhookBinSubdir)
}

// initWebhookDirs creates the framework's own directories. It only
// touches `workspace/webhook/_bin/` (compiled .go plugin cache); user
// directories — download roots, output trees, anything domain-specific
// — are NOT auto-created here. Plugins are responsible for their own
// output paths.
func initWebhookDirs() {
	if err := os.MkdirAll(webhookBinDir(), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] webhook: mkdir _bin failed: %v\n", appName, err)
	}
}

// ── Plugin discovery ───────────────────────────────────────────────────────

type pluginInfo struct {
	Path   string // absolute path to plugin file
	Action string // basename without extension
	Ext    string // ".py" / ".go" / ".sh" / ... or "" for no extension
}

// validAction enforces a strict charset for action names. Action *is*
// the filename basename, so anything outside [a-zA-Z0-9_-] is rejected
// before we ever touch the filesystem.
func validAction(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// resolvePlugin locates the plugin file matching `action`. Returns
// errPluginNotFound when no candidate exists, or a wrapped error when
// multiple files match the same basename (config error — we refuse to
// silently pick one).
func resolvePlugin(action string) (*pluginInfo, error) {
	if !validAction(action) {
		return nil, fmt.Errorf("invalid action %q", action)
	}
	dir := webhookDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errPluginNotFound
		}
		return nil, err
	}

	var matches []*pluginInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip housekeeping files (`_bin/` is a dir but we double-guard;
		// `_router.yaml`, `.gitignore`, etc. are out-of-band).
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		ext := filepath.Ext(name)
		base := strings.TrimSuffix(name, ext)
		if base != action {
			continue
		}
		switch ext {
		case "", ".sh", ".py", ".go", ".js", ".ts":
			matches = append(matches, &pluginInfo{
				Path:   filepath.Join(dir, name),
				Action: action,
				Ext:    ext,
			})
		default:
			// Unknown extension: ignore (could be docs, README, etc.)
		}
	}
	if len(matches) == 0 {
		return nil, errPluginNotFound
	}
	if len(matches) > 1 {
		var paths []string
		for _, m := range matches {
			paths = append(paths, filepath.Base(m.Path))
		}
		return nil, fmt.Errorf("multiple plugins for action %q: %s (rename or delete extras)",
			action, strings.Join(paths, ", "))
	}
	return matches[0], nil
}

// ── Plugin execution ───────────────────────────────────────────────────────

// pluginCommand returns the exec.Cmd that runs the plugin, picking an
// interpreter by extension or honoring shebang+chmod+x. The caller
// re-wraps with exec.CommandContext for timeout enforcement.
func pluginCommand(p *pluginInfo) (*exec.Cmd, error) {
	// .go: build-and-cache to _bin/<action>, then exec the binary.
	if p.Ext == ".go" {
		bin, err := compileGoPlugin(p)
		if err != nil {
			return nil, err
		}
		return exec.Command(bin), nil
	}

	info, err := os.Stat(p.Path)
	if err != nil {
		return nil, err
	}
	executable := info.Mode()&0o111 != 0

	// Honor shebang when the file is chmod+x — most flexible path: lets
	// the plugin author pick its own interpreter (`#!/usr/bin/env bun`).
	if executable {
		first, _ := readFirstLine(p.Path)
		if strings.HasPrefix(first, "#!") {
			return exec.Command(p.Path), nil
		}
	}

	switch p.Ext {
	case ".py":
		return exec.Command("python3", p.Path), nil
	case ".sh":
		return exec.Command("bash", p.Path), nil
	case ".js":
		return exec.Command("node", p.Path), nil
	case ".ts":
		// bun run handles TS natively; plugins that prefer node+ts-node can
		// just shebang-line their own interpreter.
		return exec.Command("bun", "run", p.Path), nil
	case "":
		// Bare binary — must be chmod+x.
		if !executable {
			return nil, fmt.Errorf("plugin %s has no extension and is not executable", p.Path)
		}
		return exec.Command(p.Path), nil
	}
	return nil, fmt.Errorf("unsupported plugin extension: %s", p.Ext)
}

// readFirstLine reads up to 256 bytes from the file and returns whatever
// precedes the first '\n'. Used only for shebang detection.
func readFirstLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	line := string(buf[:n])
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return strings.TrimRight(line, "\r"), nil
}

// compileGoPlugin builds <action>.go to webhook/_bin/<action>, with a
// crude mtime cache: rebuild only when the source is newer than the
// binary. This keeps Go plugins fast (sub-millisecond exec after the
// first build) without external tooling.
//
// On build failure we return the combined go-build output so the
// fallback session sees what's wrong.
func compileGoPlugin(p *pluginInfo) (string, error) {
	if err := os.MkdirAll(webhookBinDir(), 0o700); err != nil {
		return "", err
	}
	binPath := filepath.Join(webhookBinDir(), p.Action)

	srcStat, err := os.Stat(p.Path)
	if err != nil {
		return "", err
	}
	if binStat, err := os.Stat(binPath); err == nil && binStat.ModTime().After(srcStat.ModTime()) {
		return binPath, nil
	}

	cmd := exec.Command("go", "build", "-o", binPath, p.Path)
	cmd.Dir = webhookDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go build failed: %v\n%s", err, string(out))
	}
	return binPath, nil
}

// webhookResult bundles the outcome of running one plugin. Used by both
// the HTTP handler (to decide success vs fallback) and the audit logger.
type webhookResult struct {
	PluginPath string
	ExitCode   int
	Duration   time.Duration
	Stdout     []byte
	Stderr     []byte
	TimedOut   bool

	// ParsedJSON is populated when stdout is valid JSON. Plugins should
	// emit `{"status":"ok|error",...}` per the documented protocol.
	ParsedJSON map[string]any
	Status     string
}

// runPlugin executes the plugin with `body` on stdin, capturing stdout/
// stderr (each capped to webhookMaxStdoutBytes), with a deadline of
// `timeout`. Errors here represent setup failures (couldn't fork etc.);
// process-level failures are recorded on the result and returned with
// nil error — callers branch on res.ExitCode/TimedOut.
func runPlugin(parent context.Context, p *pluginInfo, body []byte, env map[string]string, timeout time.Duration) (*webhookResult, error) {
	tmpl, err := pluginCommand(p)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	// Re-build via CommandContext so timeout actually kills the child.
	args := tmpl.Args
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = webhookDir()
	cmd.Env = buildPluginEnv(env)
	cmd.Stdin = bytes.NewReader(body)

	stdout := &cappedBuffer{cap: webhookMaxStdoutBytes}
	stderr := &cappedBuffer{cap: webhookMaxStdoutBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)

	res := &webhookResult{
		PluginPath: p.Path,
		Duration:   dur,
		Stdout:     bytes.TrimRight(stdout.Bytes(), "\n"),
		Stderr:     stderr.Bytes(),
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
	} else if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		} else {
			// e.g. binary missing, fork failure
			return nil, runErr
		}
	}

	// Best-effort JSON parse. Plugins are *expected* to emit JSON but
	// we don't fail the whole call if they didn't — we just won't have
	// structured fields to forward to the client.
	if len(res.Stdout) > 0 {
		var parsed map[string]any
		if json.Unmarshal(res.Stdout, &parsed) == nil {
			res.ParsedJSON = parsed
			if s, ok := parsed["status"].(string); ok {
				res.Status = s
			}
		}
	}
	return res, nil
}

// cappedBuffer is an io.Writer with a hard byte limit. Used to bound
// stdout/stderr capture per-call — a runaway plugin can't OOM the server.
type cappedBuffer struct {
	bytes.Buffer
	cap     int
	dropped int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := c.cap - c.Buffer.Len()
	if remaining <= 0 {
		c.dropped += len(p)
		return len(p), nil // pretend we wrote so the child doesn't block
	}
	if len(p) > remaining {
		c.Buffer.Write(p[:remaining])
		c.dropped += len(p) - remaining
		return len(p), nil
	}
	return c.Buffer.Write(p)
}

// buildPluginEnv constructs the env slice for the plugin process. We
// reset PATH to a known-safe minimum (homebrew + system) so plugins
// don't inherit weird local state. Only framework-level env vars are
// guaranteed; anything domain-specific is the plugin's responsibility
// to derive from the JSON body it receives on stdin.
func buildPluginEnv(extras map[string]string) []string {
	base := []string{
		"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin",
		"HOME=" + os.Getenv("HOME"),
		"LANG=" + ifEmpty(os.Getenv("LANG"), "en_US.UTF-8"),
		"TZ=" + os.Getenv("TZ"),
	}
	for k, v := range extras {
		base = append(base, k+"="+v)
	}
	return base
}

// ── Audit log ──────────────────────────────────────────────────────────────

type webhookAuditRow struct {
	Timestamp         string
	RequestID         string
	Action            string
	PluginPath        string
	ExitCode          int
	DurationMs        int64
	FallbackSessionID string
	BodyJSON          string
	Stdout            string
	StderrTail        string
	TimedOut          bool
	Status            string // ok | error | fallback
	RemoteAddr        string
}

func recordWebhookAudit(audit webhookAuditRow) {
	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] webhook: openDB for audit failed: %v\n", appName, err)
		return
	}
	defer db.Close()

	_, err = db.Exec(`INSERT INTO webhook_audit (
		timestamp, request_id, action, plugin_path, exit_code,
		duration_ms, fallback_session_id, body_json, stdout, stderr_tail,
		timed_out, status, remote_addr
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		audit.Timestamp, audit.RequestID, audit.Action, audit.PluginPath,
		audit.ExitCode, audit.DurationMs, audit.FallbackSessionID, audit.BodyJSON,
		audit.Stdout, audit.StderrTail, audit.TimedOut, audit.Status, audit.RemoteAddr,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] webhook: audit insert failed: %v\n", appName, err)
	}
}

// ── HTTP handler ───────────────────────────────────────────────────────────

// handleWebhook is the body of POST /webhook. It does:
//
//  1. parse + validate the JSON body
//  2. read action string (required — framework does NOT infer it from
//     URL host or other body fields; that's a plugin-side concern)
//  3. resolve the plugin file
//  4. run it with stdin/stdout JSON protocol
//  5. on success: 200 + plugin's parsed result
//  6. on miss/fail/timeout: 202 + spawn AI fallback session
//
// Auth is enforced at the mux layer via authMiddleware(cfg.Token, ...);
// no separate token logic lives here.
func handleWebhook(sm *sessionManager, w http.ResponseWriter, r *http.Request) {
	bodyReader := io.LimitReader(r.Body, webhookMaxBodyBytes)
	body, err := io.ReadAll(bodyReader)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body: " + err.Error()})
		return
	}
	if len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty body"})
		return
	}

	var top map[string]any
	if err := json.Unmarshal(body, &top); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	action, _ := top["action"].(string)

	requestID := newRequestID()
	timestamp := time.Now().UTC().Format(time.RFC3339)

	audit := webhookAuditRow{
		Timestamp:  timestamp,
		RequestID:  requestID,
		Action:     action,
		BodyJSON:   truncateString(string(body), webhookAuditFieldMax),
		RemoteAddr: r.RemoteAddr,
	}

	fmt.Fprintf(os.Stderr, "[%s] webhook: %s action=%q\n", appName, requestID, action)

	// ── Resolve plugin ──
	var p *pluginInfo
	var lookupErr error
	if action != "" {
		p, lookupErr = resolvePlugin(action)
	} else {
		// No action means the body was missing the only field we route on.
		// We don't try to be clever here (no host inference, no defaults) —
		// the caller should fix the body, or wire a plugin to handle the
		// "default" / "unknown" action explicitly.
		audit.Status = "error"
		audit.StderrTail = "missing action field"
		recordWebhookAudit(audit)
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"request_id": requestID,
			"error":      "request body must include an `action` field (string)",
		})
		return
	}

	if lookupErr != nil && !errors.Is(lookupErr, errPluginNotFound) {
		// Hard config error (multi-match, invalid action). Don't fallback —
		// surface it so the caller knows their setup is broken.
		audit.Status = "error"
		audit.StderrTail = lookupErr.Error()
		recordWebhookAudit(audit)
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"request_id": requestID,
			"error":      lookupErr.Error(),
		})
		return
	}

	if errors.Is(lookupErr, errPluginNotFound) {
		// Plugin missing → spawn AI fallback session.
		sessionID := triggerFallback(sm, fallbackContext{
			RequestID: requestID,
			Action:    action,
			Body:      body,
			Reason:    "plugin not found",
		})
		audit.Status = "fallback"
		audit.FallbackSessionID = sessionID
		recordWebhookAudit(audit)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"request_id": requestID,
			"status":     "fallback",
			"reason":     "plugin not found",
			"session_id": sessionID,
		})
		return
	}

	audit.PluginPath = p.Path

	// ── Run plugin ──
	timeout := webhookDefaultTimeout
	if v, ok := top["timeout"].(float64); ok && v > 0 {
		req := time.Duration(v) * time.Second
		if req > webhookMaxTimeout {
			req = webhookMaxTimeout
		}
		timeout = req
	}

	env := pluginEnvFor(requestID, action)
	res, runErr := runPlugin(r.Context(), p, body, env, timeout)
	if runErr != nil {
		audit.Status = "error"
		audit.StderrTail = runErr.Error()
		recordWebhookAudit(audit)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"request_id": requestID,
			"error":      runErr.Error(),
		})
		return
	}

	audit.ExitCode = res.ExitCode
	audit.DurationMs = res.Duration.Milliseconds()
	audit.Stdout = truncateString(string(res.Stdout), webhookAuditFieldMax)
	audit.StderrTail = truncateString(string(res.Stderr), webhookAuditFieldMax)
	audit.TimedOut = res.TimedOut

	// ── Decide success vs fallback ──
	switch {
	case res.TimedOut:
		sessionID := triggerFallback(sm, fallbackContext{
			RequestID:  requestID,
			Action:     action,
			Body:       body,
			Reason:     fmt.Sprintf("plugin timed out after %s", timeout),
			ExitCode:   res.ExitCode,
			Stderr:     string(res.Stderr),
			TimedOut:   true,
			PluginPath: p.Path,
		})
		audit.Status = "fallback"
		audit.FallbackSessionID = sessionID
		recordWebhookAudit(audit)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"request_id": requestID,
			"status":     "fallback",
			"reason":     "timeout",
			"session_id": sessionID,
		})

	case res.ExitCode == webhookFallbackExitCode:
		sessionID := triggerFallback(sm, fallbackContext{
			RequestID:  requestID,
			Action:     action,
			Body:       body,
			Reason:     "plugin signaled fallback (exit 64)",
			ExitCode:   res.ExitCode,
			Stderr:     string(res.Stderr),
			PluginPath: p.Path,
		})
		audit.Status = "fallback"
		audit.FallbackSessionID = sessionID
		recordWebhookAudit(audit)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"request_id": requestID,
			"status":     "fallback",
			"reason":     "plugin requested fallback",
			"session_id": sessionID,
		})

	case res.ExitCode != 0:
		sessionID := triggerFallback(sm, fallbackContext{
			RequestID:  requestID,
			Action:     action,
			Body:       body,
			Reason:     fmt.Sprintf("plugin exited %d", res.ExitCode),
			ExitCode:   res.ExitCode,
			Stderr:     string(res.Stderr),
			PluginPath: p.Path,
		})
		audit.Status = "fallback"
		audit.FallbackSessionID = sessionID
		recordWebhookAudit(audit)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"request_id": requestID,
			"status":     "fallback",
			"reason":     fmt.Sprintf("exit %d", res.ExitCode),
			"session_id": sessionID,
		})

	default:
		// Success.
		audit.Status = ifEmpty(res.Status, "ok")
		recordWebhookAudit(audit)
		if res.ParsedJSON != nil {
			res.ParsedJSON["request_id"] = requestID
			writeJSON(w, http.StatusOK, res.ParsedJSON)
		} else {
			writeJSON(w, http.StatusOK, map[string]any{
				"request_id": requestID,
				"status":     "ok",
				"stdout":     string(res.Stdout),
			})
		}
	}
}

// pluginEnvFor builds the env-var map injected into plugin processes.
// Only framework-scoped variables go here. Anything domain-specific
// (download roots, host aliases, output paths, …) must be derived by
// the plugin from the JSON body or its own internal config.
//
// Documented contract — adding a new key here is non-breaking; removing
// or renaming is a breaking change.
func pluginEnvFor(requestID, action string) map[string]string {
	serverURL := fmt.Sprintf("http://127.0.0.1:%d", serverPort)
	return map[string]string{
		"WEIRAN_REQUEST_ID":  requestID,
		"WEIRAN_HOOK_ACTION": action,
		"WEIRAN_SERVER_URL":  serverURL,
		"WEIRAN_AUTH_TOKEN":  serverAuthToken,
	}
}

// ── Fallback session ───────────────────────────────────────────────────────

type fallbackContext struct {
	RequestID  string
	Action     string
	Body       []byte
	Reason     string
	ExitCode   int
	Stderr     string
	TimedOut   bool
	PluginPath string // empty when plugin couldn't be located at all
}

// triggerFallback spawns an AI fallback session and returns its ID
// (empty on spawn failure, in which case the caller should still
// audit-log the attempt). The session is given a generic directive in
// InitialMessage that tells it to (a) complete the request by any
// means, (b) repair or create the plugin in-place, then self-destruct.
//
// The directive is intentionally domain-agnostic — the fallback session
// works out what the action *means* by reading the JSON body and any
// existing plugin source.
func triggerFallback(sm *sessionManager, fc fallbackContext) string {
	sessionName := fmt.Sprintf("webhook-fallback-%s-%s",
		ifEmpty(fc.Action, "unknown"),
		time.Now().Format("0102-1504"))

	tags := []string{"webhook", "fallback"}
	if fc.Action != "" {
		tags = append(tags, "action:"+fc.Action)
	}

	initialMsg := buildFallbackMessage(fc)

	sess, err := sm.createSessionWithOpts(sessionCreateOpts{
		Name:           sessionName,
		Project:        workspace,
		Soul:           true,
		Category:       "webhook-fallback",
		Tags:           tags,
		InitialMessage: initialMsg,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] webhook: fallback spawn failed: %v\n", appName, err)
		return ""
	}

	// Capture FirstMsg synchronously so the snapshot we exposed to the
	// caller via session list reflects the fallback context immediately.
	cleanInitial, _ := sanitizeSoulPatchInput(initialMsg)
	sess.mu.Lock()
	if sess.FirstMsg == "" {
		sess.FirstMsg = cleanInitial
	}
	sess.mu.Unlock()

	// Send initial message after Claude finishes init handshake.
	go func() {
		if !sess.awaitBackendReady(30 * time.Second) {
			return
		}
		injection := sess.prepareSoulPatch(cleanInitial)
		userEvent, _ := json.Marshal(map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": injection.DisplayMessage},
		})
		sess.broadcaster.broadcast(sseEvent{Event: "user", Data: userEvent})
		if err := sess.process.sendMessage(injection.Outbound); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] webhook: fallback %s sendMessage failed: %v\n",
				appName, shortID(sess.ID), err)
			return
		}
		sess.commitSoulPatchInjection(injection)
	}()

	return sess.ID
}

// buildFallbackMessage assembles the directive that becomes the fallback
// session's first user message. The wording is intentionally generic —
// the session is told what failed and where the plugin lives, and is
// expected to figure out the rest from the JSON body + plugin source +
// any user-side conventions (which it can read from workspace).
func buildFallbackMessage(fc fallbackContext) string {
	timedOutStr := "no"
	if fc.TimedOut {
		timedOutStr = "yes"
	}

	stderrTail := fc.Stderr
	if len(stderrTail) > 4000 {
		stderrTail = "...(truncated)\n" + stderrTail[len(stderrTail)-4000:]
	}
	if stderrTail == "" {
		stderrTail = "(empty)"
	}

	pluginPath := fc.PluginPath
	if pluginPath == "" {
		// Synthesize the expected-but-missing path so the session knows
		// where to create a new plugin if it grows one.
		pluginPath = filepath.Join(webhookDir(), fc.Action+".(none — create with .sh/.py/.go/.js/.ts)")
	}

	return fmt.Sprintf(`你以 webhook fallback 模式被唤醒。

## 失败的 webhook 调用

- action: `+"`"+`%s`+"`"+`
- plugin: `+"`"+`%s`+"`"+`
- request_id: %s
- exit_code: %d
- timed_out: %s
- reason: %s

## 请求体

`+"```json\n%s\n```"+`

stderr 尾部:

`+"```\n%s\n```"+`

## 你要做两件事

**1. 完成本次请求**（plugin 没干成的事，你想办法干完）

- 业务含义在请求体里，框架不知道这个 action 是什么意思
- 输出路径、host 解析、目录布局等用户偏好都在 `+"`"+`workspace/webhook/`+"`"+` 同目录或 README 里查
- 完成后用 `+"`"+`weiran notify`+"`"+` 通知主人结果（一句话即可）

**2. 修复或新建 plugin**

- 路径: `+"`"+`%s`+"`"+`
- 协议: stdin = 完整 JSON body / stdout = `+"`"+`{"status":"ok|error","message":"..."}`+"`"+` / exit 0 = 成功
- 业务逻辑全部由 plugin 自己处理，framework 只 dispatch 不解释 action 含义
- 修不动就在文件顶部加 `+"`"+`# FIXME: <症状>`+"`"+` 标记
- 看 `+"`"+`workspace/webhook/README.md`+"`"+` 找协议详情和现有 plugin 的写法

完成后用 `+"`"+`weiran session close $WEIRAN_SESSION_ID`+"`"+` 自我销毁。
不写报告，不等确认，只做事 + notify。`,
		fc.Action,
		pluginPath,
		fc.RequestID,
		fc.ExitCode,
		timedOutStr,
		fc.Reason,
		prettyJSON(fc.Body),
		stderrTail,
		pluginPath,
	)
}

// ── Webhook tail (CLI) ─────────────────────────────────────────────────────

// runWebhookTail backs `weiran webhook tail`. Prints the last N rows of
// webhook_audit in reverse-chronological order. Surfaces fallback session
// IDs so you can `weiran session read <id>` to see what the AI did.
func runWebhookTail(args []string) {
	limit := 20
	verbose := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-n", "--limit":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &limit)
				i++
			}
		case "-v", "--verbose":
			verbose = true
		}
	}
	if limit <= 0 {
		limit = 20
	}

	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "openDB: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT timestamp, request_id, action, plugin_path,
		exit_code, duration_ms, fallback_session_id, status, timed_out, stderr_tail
		FROM webhook_audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	type row struct {
		ts, rid, action, plugin, status, stderrTail, fallbackID string
		exitCode                                                int
		durMs                                                   int64
		timedOut                                                bool
	}
	var collected []row
	for rows.Next() {
		var r row
		var to int
		if err := rows.Scan(&r.ts, &r.rid, &r.action, &r.plugin,
			&r.exitCode, &r.durMs, &r.fallbackID, &r.status, &to, &r.stderrTail); err != nil {
			continue
		}
		r.timedOut = to != 0
		collected = append(collected, r)
	}

	if len(collected) == 0 {
		fmt.Println("(no webhook audit rows yet)")
		return
	}

	for i := len(collected) - 1; i >= 0; i-- { // chrono order
		r := collected[i]
		statusTag := r.status
		if r.timedOut {
			statusTag += "/TIMEOUT"
		}
		fmt.Printf("%s  %s  action=%s  exit=%d  %dms  status=%s",
			r.ts, r.rid, ifEmpty(r.action, "-"),
			r.exitCode, r.durMs, statusTag)
		if r.fallbackID != "" {
			fmt.Printf("  fallback=%s", shortID(r.fallbackID))
		}
		fmt.Println()
		if verbose && r.stderrTail != "" {
			fmt.Printf("    plugin: %s\n", r.plugin)
			for _, line := range strings.Split(strings.TrimRight(r.stderrTail, "\n"), "\n") {
				fmt.Printf("    | %s\n", line)
			}
		}
	}
}

// runWebhookCmd dispatches `weiran webhook <sub>`.
func runWebhookCmd(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: weiran webhook <tail|list>")
		fmt.Println("  tail [-n N] [-v]   show recent webhook invocations")
		fmt.Println("  list               list discovered plugins")
		os.Exit(1)
	}
	switch args[0] {
	case "tail":
		runWebhookTail(args[1:])
	case "list":
		runWebhookList()
	default:
		fmt.Fprintf(os.Stderr, "unknown webhook subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

// runWebhookList prints what's discoverable in workspace/webhook/.
func runWebhookList() {
	dir := webhookDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("(no webhook directory at %s — create it and drop plugins in)\n", dir)
			return
		}
		fmt.Fprintf(os.Stderr, "read %s: %v\n", dir, err)
		os.Exit(1)
	}
	any := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		ext := filepath.Ext(name)
		base := strings.TrimSuffix(name, ext)
		switch ext {
		case "", ".sh", ".py", ".go", ".js", ".ts":
			info, _ := e.Info()
			perm := ""
			if info != nil && info.Mode()&0o111 != 0 {
				perm = " +x"
			}
			fmt.Printf("  %-24s  action=%s  ext=%s%s\n", name, base, ifEmpty(ext, "(none)"), perm)
			any = true
		}
	}
	if !any {
		fmt.Printf("(no plugins in %s — files are <action>.<ext>; ext in .sh .py .go .js .ts or chmod+x bare binary)\n", dir)
	}
}

// ── Small utilities ────────────────────────────────────────────────────────

func newRequestID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("wreq-%d", time.Now().UnixNano())
	}
	return "wreq-" + hex.EncodeToString(b[:])
}

func ifEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

func prettyJSON(body []byte) string {
	var v any
	if json.Unmarshal(body, &v) == nil {
		if pretty, err := json.MarshalIndent(v, "", "  "); err == nil {
			return string(pretty)
		}
	}
	return string(body)
}

// suppress "imported and not used" if our helpers shrink — these are used
// transitively by readFirstLine/openDB above.
var _ = sql.ErrNoRows
