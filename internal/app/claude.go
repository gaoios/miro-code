package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kiyor/soul-cli/pkg/provider"
	"github.com/kiyor/soul-cli/pkg/provider/gemini"
	"github.com/kiyor/soul-cli/pkg/provider/ollama"
	"github.com/kiyor/soul-cli/pkg/provider/openai"
)

// ── Lock ──

func mustLock() {
	// prefer mkdir atomic lock (NFS-safe), lockfile becomes directory
	lockDir := lockfile + ".d"

	// check if existing lock is stale
	if pid, ok := readLockPid(lockDir); ok {
		if proc, err := os.FindProcess(pid); err == nil {
			if err := proc.Signal(syscall.Signal(0)); err == nil {
				fmt.Fprintf(os.Stderr, "["+appName+"] another instance is running (pid %d), skipping\n", pid)
				os.Exit(0)
			}
		}
		// stale lock, clean up
		mkdirUnlock(lockDir)
	}

	if !mkdirLock(lockDir) {
		// mkdir failed but readLockPid also failed → possibly a leftover empty directory
		os.RemoveAll(lockDir)
		if !mkdirLock(lockDir) {
			fmt.Fprintf(os.Stderr, "["+appName+"] unable to acquire lock: %s\n", lockDir)
			os.Exit(1)
		}
	}

	// compatibility: also write old-format lockfile (for old version detection)
	os.WriteFile(lockfile, []byte(strconv.Itoa(os.Getpid())), 0644)

	// register signal handler to auto-clean lock files on kill
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\n["+appName+"] received signal %v, cleaning up lock files\n", sig)
		releaseLock()
		os.Exit(130) // 128 + SIGINT(2)
	}()
}

func releaseLock() {
	mkdirUnlock(lockfile + ".d")
	os.Remove(lockfile) // compatibility with old format
}

// ── exec claude ──

// execClaude replaces current process (for interactive/print modes, does not return).
// Exception: if an embedded OpenAI proxy is active, uses subprocess mode instead of
// syscall.Exec — exec replaces the process image and kills the proxy goroutine.
func execClaude(args []string) {
	os.Chdir(workspace)

	bin, err := exec.LookPath(claudeBin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] claude not found: %v\n", err)
		os.Exit(1)
	}

	env := injectProxyEnv(os.Environ())

	// Debug: show auth-related env vars
	for _, v := range env {
		if strings.HasPrefix(v, "ANTHROPIC_") || strings.HasPrefix(v, "CLAUDE_CODE_ENTRYPOINT=") || strings.HasPrefix(v, "CLAUDE_CODE_OAUTH") {
			if strings.HasPrefix(v, "ANTHROPIC_API_KEY=") || strings.HasPrefix(v, "CLAUDE_CODE_OAUTH_TOKEN=") {
				fmt.Fprintf(os.Stderr, "[%s] env: %s=<set>\n", appName, strings.SplitN(v, "=", 2)[0])
			} else {
				fmt.Fprintf(os.Stderr, "[%s] env: %s\n", appName, v)
			}
		}
	}

	// If an embedded provider proxy (openai/gemini/ollama) is running, we
	// cannot use syscall.Exec: exec replaces the entire process image, killing
	// the proxy goroutine before Claude Code can connect. Subprocess mode
	// keeps the proxy goroutine alive.
	if provider.ActivePort > 0 {
		cmd := exec.Command(bin, args...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = env
		if err := cmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				os.Exit(exitErr.ExitCode())
			}
			fmt.Fprintf(os.Stderr, "["+appName+"] claude failed: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	argv := append([]string{"claude"}, args...)
	if err := syscall.Exec(bin, argv, env); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] exec failed: %v\n", err)
		os.Exit(1)
	}
}

// execClaudeResume directly resumes a session (no soul prompt added, already in session).
// Any extra args (e.g. --chrome) are passed through to claude.
func execClaudeResume(sessionID string, passthrough ...string) {
	args := []string{"--dangerously-skip-permissions", "--resume", sessionID}
	args = append(args, passthrough...)
	if len(passthrough) > 0 {
		fmt.Fprintf(os.Stderr, "["+appName+"] resuming session %s with %v...\n", shortID(sessionID), passthrough)
	} else {
		fmt.Fprintf(os.Stderr, "["+appName+"] resuming session %s...\n", shortID(sessionID))
	}
	execClaude(args)
}

// runClaude runs claude as subprocess (for cron/heartbeat, returns after completion, continues with hooks)
// cron/heartbeat have timeout protection (default 10 min) to prevent infinite waits when proxy is unavailable.
func runClaude(args []string) int {
	os.Chdir(workspace)

	cmd := exec.Command(claudeBin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	// Tee stderr: real-time output to terminal + capture tail for event classification
	var stderrBuf limitedBuffer
	stderrBuf.limit = 4096 // keep last 4KB of stderr for classification
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)
	cmd.Env = append(injectProxyEnv(os.Environ()), "CLAUDE_CODE_ENTRYPOINT=sdk-cli")

	// timeout protection: set timeout for cron/heartbeat modes
	timeout := cronTimeout()

	fmt.Fprintf(os.Stderr, "["+appName+"] starting claude (subprocess mode, timeout=%s)\n", timeout)
	start := time.Now()

	// start subprocess
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] claude start failed: %v\n", err)
		trackClaudeExit(1, 0, err.Error(), "")
		return 1
	}

	// wait for completion or timeout
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var err error
	select {
	case err = <-done:
		// normal exit
	case <-time.After(timeout):
		// timeout: SIGTERM first, SIGKILL after 5 seconds
		fmt.Fprintf(os.Stderr, "["+appName+"] ⏰ claude timed out (%s), sending SIGTERM\n", timeout)
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err = <-done:
			// exited after SIGTERM
		case <-time.After(5 * time.Second):
			fmt.Fprint(os.Stderr, "["+appName+"] claude did not respond to SIGTERM, sending SIGKILL\n")
			cmd.Process.Kill()
			err = <-done
		}
		err = fmt.Errorf("timeout after %s", timeout)
	}

	duration := time.Since(start)

	exitCode := 0
	errMsg := ""
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			fmt.Fprintf(os.Stderr, "["+appName+"] claude exit code: %d\n", exitCode)
		} else {
			exitCode = 1
			errMsg = err.Error()
			fmt.Fprintf(os.Stderr, "["+appName+"] claude execution failed: %v\n", err)
		}
	} else {
		fmt.Fprint(os.Stderr, "["+appName+"] claude exited normally\n")
	}

	// persist exit code + event type to metrics.jsonl
	trackClaudeExit(exitCode, duration, errMsg, stderrBuf.String())
	return exitCode
}

// cronTimeout returns the timeout duration for cron/heartbeat
func cronTimeout() time.Duration {
	switch currentMode {
	case "heartbeat":
		return 5 * time.Minute
	case "cron":
		if time.Now().Weekday() == time.Sunday {
			return 20 * time.Minute // Sunday deep review needs more time
		}
		return 10 * time.Minute
	case "evolve":
		return 15 * time.Minute // self-evolution: more lenient than cron, may need compile+test
	default:
		return 30 * time.Minute // other modes: lenient timeout
	}
}

// preflight runs quick checks before starting claude in cron/heartbeat.
// Returns warning list (non-blocking, for logging only).
func preflight() []string {
	var warnings []string

	// 1. claude binary exists and is executable
	if _, err := os.Stat(claudeBin); err != nil {
		warnings = append(warnings, fmt.Sprintf("claude binary not found: %s", claudeBin))
	}

	// 2. check model endpoint reachability (quick 2s timeout)
	endpoint := getModelEndpoint()
	if endpoint != "" {
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(endpoint)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("model endpoint unreachable: %s (%v)", endpoint, err))
		} else {
			resp.Body.Close()
			if resp.StatusCode >= 500 {
				warnings = append(warnings, fmt.Sprintf("model endpoint error: %s → %d", endpoint, resp.StatusCode))
			}
		}
	}

	// 3. disk space (macOS: /tmp and workspace partition)
	var stat syscall.Statfs_t
	if err := syscall.Statfs(workspace, &stat); err == nil {
		freeGB := float64(stat.Bavail*uint64(stat.Bsize)) / (1024 * 1024 * 1024)
		if freeGB < 1.0 {
			warnings = append(warnings, fmt.Sprintf("low disk space: %.1f GB remaining", freeGB))
		}
	}

	return warnings
}

// getModelEndpoint reads available provider baseURL from config.json
// tries the first provider with a baseURL set
func getModelEndpoint() string {
	all, _ := loadAllProviders()
	for _, prov := range all {
		if prov.BaseURL != "" {
			return prov.BaseURL + "/models"
		}
	}
	return ""
}

// resolveProvider looks up a provider's endpoint config from config.json "providers" section.
// config.json is soul-cli's own config (in data/config.json).
func resolveProvider(providerName string) *provider.Config {
	all, _ := loadAllProviders()
	if prov, ok := all[providerName]; ok && (prov.BaseURL != "" || prov.Type == "openai" || prov.Type == "ollama" || prov.Type == "gemini") {
		return &prov
	}
	return nil
}

// loadAllProviders returns every provider from the first config.json found,
// along with the source path that was read.
func loadAllProviders() (map[string]provider.Config, string) {
	configPaths := []string{
		filepath.Join(appHome, "data", "config.json"),
		filepath.Join(workspace, "scripts", appName, "config.json"),
	}
	for _, p := range configPaths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg struct {
			Providers map[string]provider.Config `json:"providers"`
		}
		if json.Unmarshal(data, &cfg) == nil && len(cfg.Providers) > 0 {
			return cfg.Providers, p
		}
	}
	return nil, ""
}

// injectModelOverrideEnv injects ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN for the overrideModel provider.
// Called by injectProxyEnv when overrideModel is set (CLI mode backward compat).
// Returns the modified env and whether the override was applied.
func injectModelOverrideEnv(env []string) ([]string, bool) {
	return injectProviderEnv(env, overrideModel)
}

// injectProviderEnv injects ANTHROPIC_BASE_URL + auth env for the given model's provider.
// model can be fully qualified ("provider/model"), a native alias ("opus"), or a fuzzy name ("glm").
// Returns the modified env and whether the override was applied.
func injectProviderEnv(env []string, model string) ([]string, bool) {
	if model == "" {
		return env, false
	}

	// Fuzzy resolve if not already provider/model format
	if !strings.Contains(model, "/") {
		// Check native aliases first — those don't need provider injection
		if _, ok := nativeModelAliases[model]; ok {
			return env, false
		}
		// Try fuzzy match against provider models
		resolved, err := resolveFuzzyModel(model)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] %v\n", appName, err)
			return env, false
		}
		if resolved == "" || !strings.Contains(resolved, "/") {
			return env, false
		}
		model = resolved
	}

	parts := strings.SplitN(model, "/", 2)
	if len(parts) < 2 {
		return env, false
	}

	providerName := parts[0]
	modelName := parts[1]
	pcfg := resolveProvider(providerName)
	if pcfg == nil {
		fmt.Fprintf(os.Stderr, "[%s] provider %q not found in config.json (try `%s models`)\n", appName, providerName, appName)
		return env, false
	}

	// Validate model name against provider's whitelist.
	// Some upstreams (like MiniMax) silently fall back to a default model when
	// an unknown name is passed, which leads to "it runs but with the wrong model".
	// We warn loudly so typos get caught before the first token is generated.
	if len(pcfg.Models) > 0 {
		found := false
		for _, m := range pcfg.Models {
			if m == modelName {
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "[%s] ⚠️  model %q not registered under provider %q\n", appName, modelName, providerName)
			fmt.Fprintf(os.Stderr, "[%s]    available: ", appName)
			for i, m := range pcfg.Models {
				if i > 0 {
					fmt.Fprint(os.Stderr, ", ")
				}
				fmt.Fprintf(os.Stderr, "%s/%s", providerName, m)
			}
			fmt.Fprintln(os.Stderr)
			fmt.Fprintf(os.Stderr, "[%s]    upstream may silently fall back to a default model. (use `%s models` to list)\n", appName, appName)
		}
	}

	// Filter out conflicting env vars
	filtered := make([]string, 0, len(env)+3)
	for _, v := range env {
		if strings.HasPrefix(v, "ANTHROPIC_BASE_URL=") ||
			strings.HasPrefix(v, "ANTHROPIC_AUTH_TOKEN=") ||
			strings.HasPrefix(v, "ANTHROPIC_API_KEY=") {
			continue
		}
		filtered = append(filtered, v)
	}

	if pcfg.Type == "openai" {
		// Prefer server-managed proxy (server process is long-lived; CLI can use syscall.Exec normally).
		// Fall back to embedded proxy only when no server is running.
		var port int
		if serverPort := openai.DetectServerProxy(providerName); serverPort > 0 {
			port = serverPort
			fmt.Fprintf(os.Stderr, "[%s] using provider %q → server openai proxy http://127.0.0.1:%d\n", appName, providerName, port)
		} else {
			var err error
			port, err = openai.StartProxy(*pcfg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[%s] failed to start openai proxy: %v\n", appName, err)
				return env, false
			}
			fmt.Fprintf(os.Stderr, "[%s] using provider %q → embedded openai proxy http://127.0.0.1:%d\n", appName, providerName, port)
		}
		filtered = append(filtered, fmt.Sprintf("ANTHROPIC_BASE_URL=http://127.0.0.1:%d", port))
		filtered = append(filtered, "ANTHROPIC_AUTH_TOKEN=dummy")
	} else if pcfg.Type == "ollama" {
		// Ollama: start embedded proxy that translates Anthropic → OpenAI chat/completions.
		var port int
		if serverPort := ollama.DetectServerProxy(providerName); serverPort > 0 {
			port = serverPort
			fmt.Fprintf(os.Stderr, "[%s] using provider %q → server ollama proxy http://127.0.0.1:%d\n", appName, providerName, port)
		} else {
			var err error
			port, err = ollama.StartProxy(*pcfg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[%s] failed to start ollama proxy: %v\n", appName, err)
				return env, false
			}
			fmt.Fprintf(os.Stderr, "[%s] using provider %q → embedded ollama proxy http://127.0.0.1:%d\n", appName, providerName, port)
		}
		filtered = append(filtered, fmt.Sprintf("ANTHROPIC_BASE_URL=http://127.0.0.1:%d", port))
		filtered = append(filtered, "ANTHROPIC_AUTH_TOKEN=dummy")
	} else if pcfg.Type == "gemini" {
		// Gemini: start embedded proxy that translates Anthropic → Gemini Code Assist.
		var port int
		if serverPort := gemini.DetectServerProxy(providerName); serverPort > 0 {
			port = serverPort
			fmt.Fprintf(os.Stderr, "[%s] using provider %q → server gemini proxy http://127.0.0.1:%d\n", appName, providerName, port)
		} else {
			var err error
			port, err = gemini.StartProxy(*pcfg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[%s] failed to start gemini proxy: %v\n", appName, err)
				return env, false
			}
			fmt.Fprintf(os.Stderr, "[%s] using provider %q → embedded gemini proxy http://127.0.0.1:%d\n", appName, providerName, port)
		}
		filtered = append(filtered, fmt.Sprintf("ANTHROPIC_BASE_URL=http://127.0.0.1:%d", port))
		filtered = append(filtered, "ANTHROPIC_AUTH_TOKEN=dummy")
	} else {
		fmt.Fprintf(os.Stderr, "[%s] using provider %q → %s\n", appName, providerName, pcfg.BaseURL)
		filtered = append(filtered, "ANTHROPIC_BASE_URL="+pcfg.BaseURL)
		if pcfg.APIKey != "" {
			authEnv := pcfg.AuthEnv
			if authEnv == "" {
				authEnv = "ANTHROPIC_AUTH_TOKEN"
			}
			filtered = append(filtered, authEnv+"="+pcfg.APIKey)
		}
	}

	// Inject ANTHROPIC_SMALL_FAST_MODEL if the provider defines one.
	// This makes Claude Code's internal Haiku calls (session titles, tool summaries,
	// web fetch, etc.) go through the same provider instead of Anthropic.
	if pcfg.SmallFastModel != "" {
		filtered = append(filtered, "ANTHROPIC_SMALL_FAST_MODEL="+pcfg.SmallFastModel)
		fmt.Fprintf(os.Stderr, "[%s] small fast model → %s\n", appName, pcfg.SmallFastModel)
	}

	// Extend timeout for third-party providers (they can be slower)
	filtered = append(filtered, "API_TIMEOUT_MS=3000000")

	return filtered, true
}

// mergeInitModel merges the model reported by Claude Code's init message with the
// model originally requested by the user. Claude Code strips context-window suffixes
// like "[1m]" from the model ID in init messages, so if the user requested e.g.
// "claude-opus-4-6[1m]" and init reports "claude-opus-4-6", we preserve the original.
func mergeInitModel(current, fromInit string) string {
	if fromInit == "" {
		return current
	}
	if current == "" {
		return fromInit
	}
	// If current model has a bracketed suffix (e.g. "[1m]") and init model is
	// the same base without the suffix, keep the current (more specific) value.
	if idx := strings.Index(current, "["); idx > 0 {
		base := current[:idx]
		if strings.EqualFold(base, fromInit) || strings.EqualFold(current, fromInit) {
			return current
		}
	}
	// If current has a provider prefix (e.g. "openai/gpt-5") and fromInit
	// doesn't match the model portion, preserve current — it was explicitly set.
	if strings.Contains(current, "/") {
		return current
	}
	// If current equals fromInit (case-insensitive), no change needed.
	if strings.EqualFold(current, fromInit) {
		return current
	}
	return fromInit
}

// providerModelName extracts the model name from a "provider/model" string.
// Returns the full string if no "/" is found (Anthropic native model).
func providerModelName(model string) string {
	if i := strings.Index(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}

// isProviderModel returns true if the model string is in "provider/model" format.
func isProviderModel(model string) bool {
	return strings.Contains(model, "/")
}

// nativeModelAliases maps short names to full Anthropic model IDs.
// These are handled by Claude Code directly (no provider injection needed).
var nativeModelAliases = map[string]string{
	"opus":         "claude-opus-4-7",
	"opus[1m]":     "claude-opus-4-7[1m]",
	"opus-4-6":     "claude-opus-4-6",
	"opus-4-6[1m]": "claude-opus-4-6[1m]",
	"opus-4-7":     "claude-opus-4-7",
	"opus-4-7[1m]": "claude-opus-4-7[1m]",
	"sonnet":       "claude-sonnet-4-6",
	"sonnet[1m]":   "claude-sonnet-4-6[1m]",
	"haiku":        "claude-haiku-4-5-20251001",
}

// resolveFuzzyModel takes a user-supplied --model value and resolves it to a
// fully qualified model string. Supports:
//
//   - Exact match: "zai/glm-5.1", "minimax/MiniMax-M2.7-highspeed", "opus"
//   - Provider-only: "minimax" → first model under that provider
//   - Fuzzy substring: "highspeed" → "minimax/MiniMax-M2.7-highspeed"
//     "glm" → "zai/glm-5.1"
//     "M2.7" → "minimax/MiniMax-M2.7-highspeed"
//
// Returns ("", nil) if no match found.
func resolveFuzzyModel(input string) (string, error) {
	if input == "" {
		return "", nil
	}

	inputLower := strings.ToLower(input)

	// 1. Exact native alias (opus, sonnet, haiku)
	if resolved, ok := nativeModelAliases[input]; ok {
		return resolved, nil
	}

	// 2. Already fully qualified (contains "/") — validate provider name exists.
	// Without validation, a typo like "openai/gpt-5.5" (provider type, not name)
	// silently passes through and the spawn fails opaquely downstream.
	if strings.Contains(input, "/") {
		parts := strings.SplitN(input, "/", 2)
		provName, modelName := parts[0], parts[1]
		allProviders, _ := loadAllProviders()
		if allProviders == nil {
			// Config unloaded (e.g. test bench, fresh install) — preserve legacy pass-through
			return input, nil
		}
		if _, ok := allProviders[provName]; !ok {
			// Build hint: list providers that have a model containing modelName
			var hints []string
			for pn, prov := range allProviders {
				for _, m := range prov.Models {
					if strings.EqualFold(m, modelName) || strings.Contains(strings.ToLower(m), strings.ToLower(modelName)) {
						hints = append(hints, pn+"/"+m)
					}
				}
			}
			if len(hints) > 0 {
				return "", fmt.Errorf("unknown provider %q in %q (did you mean %s? or use bare %q to fuzzy-match)", provName, input, strings.Join(hints, ", "), modelName)
			}
			return "", fmt.Errorf("unknown provider %q in %q (try `%s models` or `%s providers`)", provName, input, appName, appName)
		}
		return input, nil
	}

	// 3. Check if it's a full native model ID (e.g. "claude-opus-4-6")
	for _, full := range nativeModelAliases {
		if input == full {
			return input, nil
		}
	}

	// 4. Build candidate list from all providers + their models
	allProviders, _ := loadAllProviders()
	type candidate struct {
		full  string // "provider/model"
		prov  string // provider name
		model string // model name under provider
	}
	var candidates []candidate

	for provName, prov := range allProviders {
		for _, m := range prov.Models {
			candidates = append(candidates, candidate{
				full:  provName + "/" + m,
				prov:  provName,
				model: m,
			})
		}
	}

	// 5. Exact provider name match → first model of that provider
	for _, c := range candidates {
		if inputLower == strings.ToLower(c.prov) {
			return c.full, nil
		}
	}

	// 6. Fuzzy: match against "model" part (case-insensitive substring)
	var matches []candidate
	for _, c := range candidates {
		if strings.Contains(strings.ToLower(c.model), inputLower) {
			matches = append(matches, c)
		}
	}
	// Also try matching against full "provider/model"
	if len(matches) == 0 {
		for _, c := range candidates {
			if strings.Contains(strings.ToLower(c.full), inputLower) {
				matches = append(matches, c)
			}
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no model matches %q (try `%s models`)", input, appName)
	case 1:
		return matches[0].full, nil
	default:
		// Multiple matches — list them and pick the shortest (most specific)
		fmt.Fprintf(os.Stderr, "[%s] ⚠️  %q is ambiguous, matched %d models:\n", appName, input, len(matches))
		for _, m := range matches {
			fmt.Fprintf(os.Stderr, "    %s\n", m.full)
		}
		// Pick the one with the shortest model name (most specific)
		best := matches[0]
		for _, m := range matches[1:] {
			if len(m.model) < len(best.model) {
				best = m
			}
		}
		fmt.Fprintf(os.Stderr, "[%s] → using %q (shortest match)\n", appName, best.full)
		return best.full, nil
	}
}

// lastClaudeSessionID is set after runClaude detects a new session.
// Used by trackClaudeExit to include session ID in metrics for crash recovery.
var lastClaudeSessionID string

// limitedBuffer keeps the last N bytes written to it (ring-style, but simple: just truncate head).
// Thread-safe: drainStderr goroutine writes while watchExit reads.
type limitedBuffer struct {
	buf   []byte
	limit int
	mu    sync.Mutex
}

func (lb *limitedBuffer) Write(p []byte) (n int, err error) {
	lb.mu.Lock()
	lb.buf = append(lb.buf, p...)
	if lb.limit > 0 && len(lb.buf) > lb.limit {
		lb.buf = lb.buf[len(lb.buf)-lb.limit:]
	}
	lb.mu.Unlock()
	return len(p), nil
}

func (lb *limitedBuffer) String() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return string(lb.buf)
}

// classifyExitEvent determines the event type from exit code, error message, and stderr output.
func classifyExitEvent(exitCode int, errMsg, stderr string) string {
	if exitCode == 0 {
		return "success"
	}

	combined := errMsg + " " + stderr

	// Check patterns from most specific to least
	patterns := []struct {
		eventType string
		keywords  []string
	}{
		{"timeout", []string{"timeout after", "timed out"}},
		{"login_expired", []string{"not logged in", "login required", "authenticate", "OAuth", "oauth", "CLAUDE_CODE_ENTRYPOINT"}},
		{"context_overflow", []string{"context window", "context length", "token limit", "max_tokens", "prompt is too long"}},
		{"rate_limit", []string{"rate limit", "429", "too many requests", "quota exceeded"}},
		{"network_error", []string{"connection refused", "no such host", "network unreachable", "ECONNREFUSED", "ETIMEDOUT"}},
		{"api_error", []string{"500 Internal Server", "502 Bad Gateway", "503 Service", "504 Gateway"}},
	}

	lower := strings.ToLower(combined)
	for _, p := range patterns {
		for _, kw := range p.keywords {
			if strings.Contains(lower, strings.ToLower(kw)) {
				return p.eventType
			}
		}
	}

	// Signal-based classification (128 + signal number)
	switch exitCode {
	case 130:
		return "interrupted" // SIGINT (Ctrl+C)
	case 134:
		return "aborted" // SIGABRT (assertion failure / core dump)
	case 137:
		return "oom_killed" // SIGKILL (often OOM)
	case 139:
		return "segfault" // SIGSEGV
	case 143:
		return "terminated" // SIGTERM
	}

	return "crash" // generic non-zero exit
}

// trackClaudeExit appends claude subprocess exit info to metrics.jsonl
func trackClaudeExit(exitCode int, duration time.Duration, errMsg, stderr string) {
	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")

	eventType := classifyExitEvent(exitCode, errMsg, stderr)

	rec := struct {
		Timestamp string  `json:"ts"`
		Mode      string  `json:"mode"`
		ExitCode  int     `json:"exit_code"`
		EventType string  `json:"event_type"`
		Duration  float64 `json:"duration_s"`
		Error     string  `json:"error,omitempty"`
		SessionID string  `json:"session_id,omitempty"`
	}{
		Timestamp: time.Now().Format(time.RFC3339),
		Mode:      currentMode,
		ExitCode:  exitCode,
		EventType: eventType,
		Duration:  duration.Seconds(),
		Error:     errMsg,
		SessionID: lastClaudeSessionID,
	}

	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	data = append(data, '\n')

	f, err := os.OpenFile(metricsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return
	}
	fi, err := f.Stat()
	f.Close()
	if err != nil {
		return
	}

	// rotate only if file is large enough to possibly exceed 1000 lines
	// (average metric line ~150 bytes, so 1000 lines ~150KB)
	if fi != nil && fi.Size() > 100*1024 {
		rotateMetrics(metricsPath, 1000, 800)
	}
}

// rotateMetrics truncates to last keepLines when file exceeds maxLines
func rotateMetrics(path string, maxLines, keepLines int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) <= maxLines {
		return
	}
	kept := lines[len(lines)-keepLines:]
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] metrics rotate write failed: %v\n", appName, err)
	}
}

// crashInfo describes a recent crash for crash recovery.
type crashInfo struct {
	Timestamp string `json:"ts"`
	Mode      string `json:"mode"`
	ExitCode  int    `json:"exit_code"`
	EventType string `json:"event_type"`
	SessionID string `json:"session_id"`
	Error     string `json:"error"`
}

// getLastCrashInfo returns info about the most recent crash for the given mode,
// or nil if the last run was successful or the crash is older than 1 hour.
func getLastCrashInfo(mode string) *crashInfo {
	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	data, err := os.ReadFile(metricsPath)
	if err != nil {
		return nil
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		var rec crashInfo
		if json.Unmarshal([]byte(lines[i]), &rec) != nil {
			continue
		}
		if rec.Mode != mode {
			continue
		}
		// Found the most recent record for this mode
		if rec.ExitCode == 0 {
			return nil // last run was successful, no recovery needed
		}
		// Check if crash is too old (>1h)
		if t, err := time.Parse(time.RFC3339, rec.Timestamp); err == nil {
			if time.Since(t) > time.Hour {
				return nil
			}
		}
		return &rec
	}
	return nil
}

// alertOnRepeatedCrash checks if the last N runs of the given mode all crashed.
// If 3+ consecutive crashes, sends a Telegram alert. Stateless — only reads metrics.
func alertOnRepeatedCrash(mode string) {
	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	data, err := os.ReadFile(metricsPath)
	if err != nil {
		return
	}

	const threshold = 3
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	consecutive := 0
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var rec crashInfo
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec.Mode != mode {
			continue
		}
		if rec.ExitCode == 0 {
			break // hit a success, streak broken
		}
		consecutive++
		if consecutive >= threshold {
			msg := fmt.Sprintf("⚠️ %s has crashed %d consecutive times. Last error: %s",
				mode, consecutive, rec.Error)
			fmt.Fprintf(os.Stderr, "[%s] %s\n", appName, msg)
			// Use trySendTelegram (non-fatal) — sendTelegram calls os.Exit on failure
			if err := trySendTelegram(msg); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] telegram alert failed: %v\n", appName, err)
			}
			return
		}
	}
}

// buildEventDigest returns a compact summary of recent events for injection into evolve task.
// Focuses on failures, especially unclassified ones (event_type=crash) that may need new patterns.
func buildEventDigest(since time.Duration) string {
	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	data, err := os.ReadFile(metricsPath)
	if err != nil {
		return ""
	}

	cutoff := time.Now().Add(-since)
	typeCounts := make(map[string]int)
	totalRuns := 0

	// Collect unique unclassified crash errors for pattern discovery
	unclassifiedErrors := make(map[string]int) // error → count

	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			Timestamp string `json:"ts"`
			Mode      string `json:"mode"`
			ExitCode  int    `json:"exit_code"`
			EventType string `json:"event_type"`
			Error     string `json:"error"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, rec.Timestamp)
		if err != nil || t.Before(cutoff) {
			continue
		}
		et := rec.EventType
		if et == "" {
			if rec.ExitCode == 0 {
				et = "success"
			} else {
				et = "crash"
			}
		}
		typeCounts[et]++
		totalRuns++

		// Track unclassified crashes for evolve to investigate
		if et == "crash" && rec.Error != "" {
			// Normalize: take first 80 chars as key
			key := rec.Error
			if len(key) > 80 {
				key = key[:80]
			}
			unclassifiedErrors[key]++
		}
	}

	if totalRuns == 0 {
		return ""
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Total: %d runs", totalRuns)
	failures := totalRuns - typeCounts["success"]
	if failures > 0 {
		fmt.Fprintf(&sb, ", %d failures", failures)
	}
	sb.WriteString("\n")

	// Only show non-success types
	for _, et := range []string{"timeout", "login_expired", "rate_limit", "network_error", "api_error", "context_overflow", "oom_killed", "segfault", "crash"} {
		if c, ok := typeCounts[et]; ok {
			fmt.Fprintf(&sb, "  %s: %d\n", et, c)
		}
	}

	// Highlight unclassified crashes — these are evolve's job to classify
	if len(unclassifiedErrors) > 0 {
		sb.WriteString("\n⚠️ Unclassified crashes (need new patterns in classifyExitEvent):\n")
		for errKey, count := range unclassifiedErrors {
			fmt.Fprintf(&sb, "  [%dx] %s\n", count, errKey)
		}
	}

	return sb.String()
}

// handleEventLog implements `weiran db events` — aggregates metrics.jsonl into a readable summary.
// Flags: --since=24h (default), --mode=cron, --type=timeout, --notify (send to Telegram)
func handleEventLog(args []string) {
	since := 24 * time.Hour
	filterMode := ""
	filterType := ""
	notify := false

	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--since="):
			if d, err := time.ParseDuration(strings.TrimPrefix(a, "--since=")); err == nil {
				since = d
			}
		case strings.HasPrefix(a, "--mode="):
			filterMode = strings.TrimPrefix(a, "--mode=")
		case strings.HasPrefix(a, "--type="):
			filterType = strings.TrimPrefix(a, "--type=")
		case a == "--notify":
			notify = true
		}
	}

	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	data, err := os.ReadFile(metricsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no metrics file: %v\n", err)
		return
	}

	cutoff := time.Now().Add(-since)

	type eventRec struct {
		Timestamp string  `json:"ts"`
		Mode      string  `json:"mode"`
		ExitCode  int     `json:"exit_code"`
		EventType string  `json:"event_type"`
		Duration  float64 `json:"duration_s"`
		Error     string  `json:"error"`
		SessionID string  `json:"session_id"`
	}

	var events []eventRec
	typeCounts := make(map[string]int)
	modeCounts := make(map[string]int)
	totalRuns := 0
	totalFails := 0

	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec eventRec
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, rec.Timestamp)
		if err != nil || t.Before(cutoff) {
			continue
		}
		if filterMode != "" && rec.Mode != filterMode {
			continue
		}
		if filterType != "" && rec.EventType != filterType {
			continue
		}
		// backfill event_type for old records without it
		if rec.EventType == "" {
			if rec.ExitCode == 0 {
				rec.EventType = "success"
			} else {
				rec.EventType = "crash"
			}
		}
		events = append(events, rec)
		typeCounts[rec.EventType]++
		modeCounts[rec.Mode]++
		totalRuns++
		if rec.ExitCode != 0 {
			totalFails++
		}
	}

	if totalRuns == 0 {
		fmt.Println("no events in the last", since)
		return
	}

	// Build summary
	var sb strings.Builder
	fmt.Fprintf(&sb, "📊 Event Log (%s)\n", since)
	fmt.Fprintf(&sb, "Total: %d runs, %d failures (%.0f%% success)\n\n",
		totalRuns, totalFails, float64(totalRuns-totalFails)/float64(totalRuns)*100)

	// By event type
	sb.WriteString("By type:\n")
	for _, et := range []string{"success", "timeout", "login_expired", "rate_limit", "network_error", "api_error", "context_overflow", "oom_killed", "segfault", "crash"} {
		if c, ok := typeCounts[et]; ok {
			fmt.Fprintf(&sb, "  %-18s %d\n", et, c)
		}
	}

	// By mode
	sb.WriteString("\nBy mode:\n")
	for _, m := range []string{"heartbeat", "cron", "evolve", "interactive"} {
		if c, ok := modeCounts[m]; ok {
			fmt.Fprintf(&sb, "  %-18s %d\n", m, c)
		}
	}

	// Recent failures (last 5)
	failCount := 0
	sb.WriteString("\nRecent failures:\n")
	for i := len(events) - 1; i >= 0 && failCount < 5; i-- {
		e := events[i]
		if e.ExitCode == 0 {
			continue
		}
		errSnippet := e.Error
		if len(errSnippet) > 60 {
			errSnippet = errSnippet[:60] + "…"
		}
		fmt.Fprintf(&sb, "  %s  %-12s %-16s %s\n", e.Timestamp[:16], e.Mode, e.EventType, errSnippet)
		failCount++
	}
	if failCount == 0 {
		sb.WriteString("  (none)\n")
	}

	summary := sb.String()
	fmt.Print(summary)

	if notify {
		if err := trySendTelegram(summary); err != nil {
			fmt.Fprintf(os.Stderr, "telegram send failed: %v\n", err)
		} else {
			fmt.Fprintln(os.Stderr, "→ sent to Telegram")
		}
	}
}
