package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// agentDef represents a spawnable agent read from config.json
type agentDef struct {
	ID        string `json:"-"`
	Name      string `json:"name"`
	Workspace string `json:"workspace"`
	Model     string `json:"model"` // claude model flag: "sonnet", "haiku", "opus", etc.
}

// loadAgents reads agent definitions from config.json's "agents" field.
func loadAgents() ([]agentDef, error) {
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
			Agents map[string]agentDef `json:"agents"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil || len(cfg.Agents) == 0 {
			continue
		}
		agents := make([]agentDef, 0, len(cfg.Agents))
		for id, a := range cfg.Agents {
			a.ID = id
			if a.Name == "" {
				a.Name = id
			}
			if a.Workspace == "" {
				a.Workspace = workspace
			} else if strings.HasPrefix(a.Workspace, "~/") {
				a.Workspace = filepath.Join(home, a.Workspace[2:])
			}
			agents = append(agents, a)
		}
		return agents, nil
	}

	return nil, fmt.Errorf("no agents defined in config.json")
}

// findAgent looks up an agent by ID (exact) or name (prefix match)
func findAgent(agents []agentDef, query string) *agentDef {
	q := strings.ToLower(query)
	// exact ID match first
	for i, a := range agents {
		if a.ID == q {
			return &agents[i]
		}
	}
	// prefix match on ID or name
	for i, a := range agents {
		if strings.HasPrefix(strings.ToLower(a.ID), q) || strings.HasPrefix(strings.ToLower(a.Name), q) {
			return &agents[i]
		}
	}
	return nil
}

// buildAgentPrompt assembles a minimal soul prompt from the agent's workspace
func buildAgentPrompt(agent *agentDef) string {
	ws := agent.Workspace
	if ws == "" {
		ws = filepath.Join(appHome, "workspace")
	}

	var b strings.Builder

	// Minimal boot protocol
	fmt.Fprintf(&b, "# Boot Protocol\n\n")
	fmt.Fprintf(&b, "Below are your identity and behavioral rules.\n")
	fmt.Fprintf(&b, "After reading, act as the persona defined in your soul files.\n\n")

	// Load soul files from agent workspace
	for _, name := range []string{"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "TOOLS.md"} {
		content, ok := loadFileWithBudget(filepath.Join(ws, name), 8000)
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "\n# === %s ===\n\n%s\n", name, content)
	}

	// HEARTBEAT.md if present (contains agent-specific task rules)
	if content, ok := loadFileWithBudget(filepath.Join(ws, "HEARTBEAT.md"), 4000); ok {
		fmt.Fprintf(&b, "\n# === HEARTBEAT.md ===\n\n%s\n", content)
	}

	// MEMORY.md index
	if content, ok := loadFileWithBudget(filepath.Join(ws, "MEMORY.md"), 4000); ok {
		fmt.Fprintf(&b, "\n# === MEMORY.md ===\n\n%s\n", content)
	}

	// Current time
	now := time.Now()
	fmt.Fprintf(&b, "\n> **Current time**: %s (%s)\n", now.Format("2006-01-02 15:04 MST"), now.Weekday())
	// Show "Nick (App)" so spawned agents see who summoned them in
	// human-readable form, plus the binary identifier for tooling.
	// When agentNick == appName (no nickname configured), show only the name.
	if agentNick != "" && agentNick != appName {
		fmt.Fprintf(&b, "\n> **Spawned by**: %s (%s)\n", agentNick, appName)
	} else {
		fmt.Fprintf(&b, "\n> **Spawned by**: %s\n", appName)
	}

	return b.String()
}

// handleSpawn implements `weiran spawn <agent> "task"` [--wait]
// and `weiran spawn --bare --model <model> --project <path> "task"` [--wait] [--name N]
func handleSpawn(args []string) {
	if len(args) >= 1 {
		switch args[0] {
		case "list", "ls":
			handleSpawnList()
			return
		case "log":
			handleSpawnLog(args[1:])
			return
		case "finish":
			handleSpawnFinish(args[1:])
			return
		}
	}

	// Parse flags from all args
	wait := false
	bare := false
	self := false
	bareModel := ""
	bareName := ""
	bareProject := ""
	bareBackend := ""
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--wait", "-w":
			wait = true
		case "--bare":
			bare = true
		case "--self":
			self = true
		case "--model":
			if i+1 < len(args) {
				i++
				bareModel = args[i]
			}
		case "--name":
			if i+1 < len(args) {
				i++
				bareName = args[i]
			}
		case "--project":
			if i+1 < len(args) {
				i++
				bareProject = args[i]
			}
		case "--backend":
			// Round 4: opt into the codex backend (or pin cc explicitly).
			// Empty / "auto" lets the server auto-route by model.
			if i+1 < len(args) {
				i++
				bareBackend = args[i]
			}
		default:
			positional = append(positional, args[i])
		}
	}

	// --bare mode: no agent needed, just model + task, no soul files
	if bare {
		if bareModel == "" {
			fmt.Fprintf(os.Stderr, "[%s] --bare requires --model <model>\n", appName)
			os.Exit(1)
		}
		// Fuzzy resolve model name (e.g. "glm" → "zai/glm-5.1").
		// codex/grok/kimi backends use native model IDs that don't live in the
		// Claude model registry, so route through resolveSpawnModel (which
		// passes those straight through) instead of always fuzzy-matching
		// against Claude aliases/providers.
		if resolved, err := resolveSpawnModel(bareModel, bareBackend); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] %v\n", appName, err)
			os.Exit(1)
		} else if resolved != "" {
			bareModel = resolved
		}
		if bareProject == "" {
			fmt.Fprintf(os.Stderr, "[%s] --bare requires --project <path>\n", appName)
			os.Exit(1)
		}
		if len(positional) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s spawn --bare --model <model> --project <path> \"task\" [--wait] [--name <name>]\n", appName)
			os.Exit(1)
		}
		task := positional[0]
		if bareName == "" {
			bareName = fmt.Sprintf("bare-%s", time.Now().Format("0102-1504"))
		}
		handleBareSpawn(bareName, bareModel, bareProject, task, wait, bareBackend)
		return
	}

	if len(positional) < 2 {
		// List available agents
		agents, err := loadAgents()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] %v\n", appName, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "usage: %s spawn <agent> \"task\" [--wait]\n", appName)
		fmt.Fprintf(os.Stderr, "       %s spawn --self main \"task\" [--wait] [--model <model>]  — spawn with own soul\n", appName)
		fmt.Fprintf(os.Stderr, "       %s spawn --bare --model <model> --project <path> [--backend cc|codex|grok|kimi|agy|auto] \"task\" [--wait]\n", appName)
		fmt.Fprintf(os.Stderr, "       %s spawn list          — show recent spawns\n", appName)
		fmt.Fprintf(os.Stderr, "       %s spawn log <id>      — view spawn output\n\n", appName)
		fmt.Fprintln(os.Stderr, "Available agents:")
		for _, a := range agents {
			model := a.Model
			if model == "" {
				model = "default"
			}
			selfTag := ""
			if a.ID == "main" {
				selfTag = " (--self)"
			}
			fmt.Fprintf(os.Stderr, "  %-12s %-10s model=%-8s workspace=%s%s\n", a.ID, a.Name, model, a.Workspace, selfTag)
		}
		os.Exit(1)
	}

	agentQuery := positional[0]
	task := positional[1]

	// --self mode: construct main agent def from current workspace
	if self {
		if agentQuery != "main" && agentQuery != "self" {
			fmt.Fprintf(os.Stderr, "[%s] --self only works with 'main' agent\n", appName)
			os.Exit(1)
		}
		agent := &agentDef{
			ID:        "main",
			Name:      appName,
			Workspace: workspace,
		}
		if bareModel != "" {
			if resolved, err := resolveSpawnModel(bareModel, bareBackend); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] %v\n", appName, err)
				os.Exit(1)
			} else if resolved != "" {
				agent.Model = resolved
			} else {
				fmt.Fprintf(os.Stderr, "[%s] error: model %q could not be resolved\n", appName, bareModel)
				os.Exit(1)
			}
		}

		// Try to delegate to server if running
		if delegateSpawnToServer(agent, task, wait) {
			return
		}

		fmt.Fprintf(os.Stderr, "[%s] spawning self (%s)...\n", appName, agent.Name)
		fmt.Fprintf(os.Stderr, "[%s]   workspace: %s\n", appName, agent.Workspace)
		if agent.Model != "" {
			fmt.Fprintf(os.Stderr, "[%s]   model: %s\n", appName, agent.Model)
		}
		fmt.Fprintf(os.Stderr, "[%s]   task: %s\n", appName, truncate(task, 80))

		// Build prompt file
		promptContent := buildAgentPrompt(agent)
		tmpDir, err := os.MkdirTemp("", fmt.Sprintf("%s-spawn-self-", appName))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] failed to create temp dir: %v\n", appName, err)
			os.Exit(1)
		}
		promptFile := filepath.Join(tmpDir, "prompt.md")
		if err := os.WriteFile(promptFile, []byte(promptContent), 0600); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] failed to write prompt: %v\n", appName, err)
			os.Exit(1)
		}

		tokens := estimateTokens(promptContent)
		fmt.Fprintf(os.Stderr, "[%s]   prompt: ~%dk tokens\n", appName, tokens/1000)

		// Build claude args
		sessionName := fmt.Sprintf("spawn-self-%s", time.Now().Format("0102-1504"))
		claudeArgs := []string{
			"--append-system-prompt-file", promptFile,
			"--dangerously-skip-permissions",
			"--name", sessionName,
			"-p", task,
		}
		if agent.Model != "" {
			claudeArgs = append(claudeArgs, "--model", agent.Model)
		}

		// Run claude in workspace
		cmd := exec.Command(claudeBin, claudeArgs...)
		cmd.Dir = agent.Workspace
		cmd.Env = injectProxyEnv(os.Environ())

		// Set JIRA_TOKEN from workspace .jira-token file
		jiraTokenFile := filepath.Join(agent.Workspace, ".jira-token")
		if data, err := os.ReadFile(jiraTokenFile); err == nil {
			token := strings.TrimSpace(string(data))
			if token != "" {
				cmd.Env = append(cmd.Env, "JIRA_TOKEN="+token)
			}
		}

		logFile := filepath.Join(tmpDir, "output.log")
		spawnID := recordSpawnStart(agent, task, sessionName, tmpDir)

		if wait {
			fmt.Fprintf(os.Stderr, "[%s] running synchronously...\n", appName)
			out, err := cmd.CombinedOutput()
			os.WriteFile(logFile, out, 0600)
			exitCode := 0
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					exitCode = exitErr.ExitCode()
				} else {
					exitCode = 1
				}
			}
			recordSpawnFinish(spawnID, exitCode, 0, "")
			fmt.Print(string(out))
		} else {
			f, _ := os.Create(logFile)
			cmd.Stdout = f
			cmd.Stderr = f
			if err := cmd.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] failed to start: %v\n", appName, err)
				os.Exit(1)
			}
			go func() {
				exitCode := 0
				if err := cmd.Wait(); err != nil {
					if exitErr, ok := err.(*exec.ExitError); ok {
						exitCode = exitErr.ExitCode()
					} else {
						exitCode = 1
					}
				}
				f.Close()
				recordSpawnFinish(spawnID, exitCode, 0, "")
			}()
			fmt.Fprintf(os.Stderr, "[%s] spawned self (async, id=%d)\n", appName, spawnID)
			fmt.Printf("spawn_ok id=%d agent=main session=%s log=%s\n", spawnID, sessionName, logFile)
		}
		return
	}

	agents, err := loadAgents()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] %v\n", appName, err)
		os.Exit(1)
	}

	agent := findAgent(agents, agentQuery)
	if agent == nil {
		fmt.Fprintf(os.Stderr, "[%s] agent not found: %s\n", appName, agentQuery)
		fmt.Fprintln(os.Stderr, "Available:")
		for _, a := range agents {
			fmt.Fprintf(os.Stderr, "  %s (%s)\n", a.ID, a.Name)
		}
		fmt.Fprintf(os.Stderr, "\nTo spawn with own soul: %s spawn --self main \"task\"\n", appName)
		os.Exit(1)
	}

	// Per-agent mutual exclusion: reject if this agent already has a running spawn
	if running := agentRunningSpawn(agent.ID); running != nil {
		fmt.Fprintf(os.Stderr, "[%s] agent %s already has a running spawn (id=%d, task=%q, started=%s)\n",
			appName, agent.ID, running.id, truncate(running.task, 60), running.started)
		fmt.Fprintf(os.Stderr, "[%s] use '%s spawn list' to check status\n", appName, appName)
		os.Exit(1)
	}

	// Try to delegate to server if running
	if delegateSpawnToServer(agent, task, wait) {
		return
	}

	fmt.Fprintf(os.Stderr, "[%s] spawning %s (%s)...\n", appName, agent.Name, agent.ID)
	fmt.Fprintf(os.Stderr, "[%s]   workspace: %s\n", appName, agent.Workspace)
	if agent.Model != "" {
		fmt.Fprintf(os.Stderr, "[%s]   model: %s\n", appName, agent.Model)
	}
	fmt.Fprintf(os.Stderr, "[%s]   task: %s\n", appName, truncate(task, 80))

	// Build prompt file
	promptContent := buildAgentPrompt(agent)
	tmpDir, err := os.MkdirTemp("", fmt.Sprintf("%s-spawn-%s-", appName, agent.ID))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] failed to create temp dir: %v\n", appName, err)
		os.Exit(1)
	}
	promptFile := filepath.Join(tmpDir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte(promptContent), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] failed to write prompt: %v\n", appName, err)
		os.Exit(1)
	}

	tokens := estimateTokens(promptContent)
	fmt.Fprintf(os.Stderr, "[%s]   prompt: ~%dk tokens\n", appName, tokens/1000)

	// Build claude args
	sessionName := fmt.Sprintf("spawn-%s-%s", agent.ID, time.Now().Format("0102-1504"))
	claudeArgs := []string{
		"--append-system-prompt-file", promptFile,
		"--dangerously-skip-permissions",
		"--name", sessionName,
		"-p", task,
	}
	if agent.Model != "" {
		claudeArgs = append(claudeArgs, "--model", agent.Model)
	}

	// Run claude in agent's workspace
	cmd := exec.Command(claudeBin, claudeArgs...)
	cmd.Dir = agent.Workspace
	cmd.Env = injectProxyEnv(os.Environ())

	// Set JIRA_TOKEN from agent's .jira-token file if it exists
	jiraTokenFile := filepath.Join(agent.Workspace, ".jira-token")
	if data, err := os.ReadFile(jiraTokenFile); err == nil {
		token := strings.TrimSpace(string(data))
		if token != "" {
			cmd.Env = append(cmd.Env, "JIRA_TOKEN="+token)
		}
	}

	start := time.Now()

	// Record spawn in DB
	spawnID := recordSpawnStart(agent, task, sessionName, tmpDir)

	if wait {
		// Synchronous: pipe stdout/stderr, also capture to log
		logFile := filepath.Join(tmpDir, "output.log")
		logF, _ := os.Create(logFile)
		cmd.Stdout = io.MultiWriter(os.Stdout, logF)
		cmd.Stderr = io.MultiWriter(os.Stderr, logF)
		err := cmd.Run()
		logF.Close()
		duration := time.Since(start)
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
		}
		fmt.Fprintf(os.Stderr, "\n[%s] spawn %s finished (exit=%d, duration=%s)\n",
			appName, agent.ID, exitCode, duration.Round(time.Second))

		recordSpawnFinish(spawnID, exitCode, duration, logFile)
		os.Exit(exitCode)
	}

	// Async: double-fork to fully detach child from parent process.
	logFile := filepath.Join(tmpDir, "output.log")
	pidFile := filepath.Join(tmpDir, "pid")
	exitFile := filepath.Join(tmpDir, "exit")

	// Build wrapper script: run claude, record result, notify
	notifyBin := os.Args[0]
	wrapperScript := fmt.Sprintf(`#!/bin/sh
echo $$ > %q
%s > %q 2>&1
EXIT_CODE=$?
echo "$EXIT_CODE" > %q
DURATION=$SECONDS
# Record to DB
%s spawn finish %d $EXIT_CODE $DURATION %q 2>/dev/null
if [ "$EXIT_CODE" -eq 0 ]; then
  %s notify "✅ spawn %s 完成 (${DURATION}s)\nsession: %s"
else
  %s notify "❌ spawn %s 失败 (exit=$EXIT_CODE, ${DURATION}s)\nsession: %s\nlog: %s"
fi
`,
		pidFile,
		shellQuoteArgs(cmd.Path, cmd.Args[1:]...), logFile,
		exitFile,
		notifyBin, spawnID, logFile,
		notifyBin, agent.Name, sessionName,
		notifyBin, agent.Name, sessionName, logFile,
	)

	wrapperPath := filepath.Join(tmpDir, "run.sh")
	if err := os.WriteFile(wrapperPath, []byte(wrapperScript), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] failed to write wrapper script: %v\n", appName, err)
		os.Exit(1)
	}

	// Launch wrapper with setsid so it survives parent exit
	bgCmd := exec.Command("/bin/sh", wrapperPath)
	bgCmd.Dir = agent.Workspace
	bgCmd.Env = cmd.Env
	bgCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	bgCmd.Stdin = nil
	bgCmd.Stdout = nil
	bgCmd.Stderr = nil

	if err := bgCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] failed to start: %v\n", appName, err)
		os.Exit(1)
	}

	bgCmd.Process.Release()

	fmt.Fprintf(os.Stderr, "[%s] spawned %s (async, id=%d)\n", appName, agent.Name, spawnID)
	fmt.Fprintf(os.Stderr, "[%s]   session: %s\n", appName, sessionName)
	fmt.Fprintf(os.Stderr, "[%s]   log: %s\n", appName, logFile)

	fmt.Printf("spawn_ok id=%d agent=%s session=%s log=%s\n", spawnID, agent.ID, sessionName, logFile)
}

// runningSpawn holds info about an active spawn for dedup checks.
type runningSpawn struct {
	id      int64
	task    string
	started string
	logDir  string
}

// agentRunningSpawn checks if the given agent has a spawn with status='running'.
// It also verifies the process is actually alive via the PID file; if not, it
// marks the stale entry as failed and returns nil.
func agentRunningSpawn(agentID string) *runningSpawn {
	db, err := openDB()
	if err != nil {
		return nil
	}
	defer db.Close()

	rows, err := db.Query(
		`SELECT id, task, started_at, log_path FROM spawns WHERE agent=? AND status='running' ORDER BY id DESC`,
		agentID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var task, started, logPath string
		rows.Scan(&id, &task, &started, &logPath)

		// Check if process is actually alive via PID file next to the log
		logDir := filepath.Dir(logPath)
		pidFile := filepath.Join(logDir, "pid")
		pidData, err := os.ReadFile(pidFile)
		if err != nil {
			// No PID file — process never started or already cleaned up
			db.Exec(`UPDATE spawns SET status='failed', finished_at=? WHERE id=?`,
				time.Now().Format(time.RFC3339), id)
			continue
		}
		pid := parseInt(strings.TrimSpace(string(pidData)))
		if pid <= 0 {
			db.Exec(`UPDATE spawns SET status='failed', finished_at=? WHERE id=?`,
				time.Now().Format(time.RFC3339), id)
			continue
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			db.Exec(`UPDATE spawns SET status='failed', finished_at=? WHERE id=?`,
				time.Now().Format(time.RFC3339), id)
			continue
		}
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			// Process is dead — mark stale entry
			db.Exec(`UPDATE spawns SET status='failed', finished_at=? WHERE id=?`,
				time.Now().Format(time.RFC3339), id)
			continue
		}
		// Process is alive
		return &runningSpawn{id: id, task: task, started: started, logDir: logDir}
	}
	return nil
}

// shellQuoteArgs builds a shell-safe command string
func shellQuoteArgs(bin string, args ...string) string {
	parts := []string{shellQuote(bin)}
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	// Single-quote the string, escaping any embedded single quotes
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// shellEscapeForDoubleQuote escapes a string for safe interpolation inside
// a double-quoted shell string. Escapes $, `, \, ", and ! characters.
func shellEscapeForDoubleQuote(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`$`, `\$`,
		"`", "\\`",
		`!`, `\!`,
	)
	return r.Replace(s)
}

// recordSpawnStart inserts a new spawn record and returns its ID
func recordSpawnStart(agent *agentDef, task, session, logDir string) int64 {
	db, err := openDB()
	if err != nil {
		return 0
	}
	defer db.Close()

	logPath := filepath.Join(logDir, "output.log")
	res, err := db.Exec(
		`INSERT INTO spawns (agent, agent_name, task, session, log_path, status, started_at)
		 VALUES (?, ?, ?, ?, ?, 'running', ?)`,
		agent.ID, agent.Name, task, session, logPath, time.Now().Format(time.RFC3339),
	)
	if err != nil {
		return 0
	}
	id, _ := res.LastInsertId()
	return id
}

// recordSpawnFinish updates a spawn record with results
func recordSpawnFinish(id int64, exitCode int, duration time.Duration, logPath string) {
	if id == 0 {
		return
	}
	db, err := openDB()
	if err != nil {
		return
	}
	defer db.Close()

	status := "done"
	if exitCode != 0 {
		status = "failed"
	}

	// Grab last 500 bytes of output for quick preview
	tail := readTail(logPath, 500)

	db.Exec(
		`UPDATE spawns SET exit_code=?, duration_s=?, status=?, output_tail=?, log_path=?, finished_at=?
		 WHERE id=?`,
		exitCode, duration.Seconds(), status, tail, logPath, time.Now().Format(time.RFC3339), id,
	)
}

// handleSpawnFinish is called from the async wrapper script: `weiran spawn finish <id> <exit_code> <duration_s> <log_path>`
func handleSpawnFinish(args []string) {
	if len(args) < 4 {
		return
	}
	id := int64(parseInt(args[0]))
	exitCode := parseInt(args[1])
	durationS := parseFloat(args[2])
	logPath := args[3]
	recordSpawnFinish(id, exitCode, time.Duration(durationS*float64(time.Second)), logPath)
}

// handleSpawnList shows spawn history from DB
func handleSpawnList() {
	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] db error: %v\n", appName, err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.Query(
		`SELECT id, agent, agent_name, task, session, status, exit_code, duration_s, started_at, finished_at
		 FROM spawns ORDER BY id DESC LIMIT 20`,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] query error: %v\n", appName, err)
		os.Exit(1)
	}
	defer rows.Close()

	fmt.Fprintf(os.Stderr, "%-4s %-10s %-10s %-8s %-6s %-8s %-20s %s\n",
		"ID", "AGENT", "NAME", "STATUS", "EXIT", "DURATION", "STARTED", "TASK")
	count := 0
	for rows.Next() {
		var id int
		var agent, agentName, task, session, status, startedAt string
		var exitCode sql.NullInt64
		var durationS sql.NullFloat64
		var finishedAt sql.NullString
		rows.Scan(&id, &agent, &agentName, &task, &session, &status, &exitCode, &durationS, &startedAt, &finishedAt)

		exitStr := "-"
		if exitCode.Valid {
			exitStr = fmt.Sprintf("%d", exitCode.Int64)
		}
		durStr := "-"
		if durationS.Valid {
			durStr = fmt.Sprintf("%.0fs", durationS.Float64)
		}
		// Parse and format start time shorter
		t, _ := time.Parse(time.RFC3339, startedAt)
		timeStr := t.Format("01/02 15:04")

		fmt.Fprintf(os.Stderr, "%-4d %-10s %-10s %-8s %-6s %-8s %-20s %s\n",
			id, agent, agentName, status, exitStr, durStr, timeStr, truncate(task, 50))
		count++
	}
	if count == 0 {
		fmt.Fprintln(os.Stderr, "No spawn records found.")
	}
}

// handleSpawnLog shows the output of a specific spawn
func handleSpawnLog(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "usage: %s spawn log <id>\n", appName)
		os.Exit(1)
	}
	id := parseInt(args[0])

	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] db error: %v\n", appName, err)
		os.Exit(1)
	}
	defer db.Close()

	var logPath, task, agent, status string
	err = db.QueryRow(`SELECT log_path, task, agent, status FROM spawns WHERE id=?`, id).
		Scan(&logPath, &task, &agent, &status)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] spawn #%d not found\n", appName, id)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "spawn #%d | agent=%s | status=%s\ntask: %s\nlog: %s\n---\n",
		id, agent, status, task, logPath)

	data, err := os.ReadFile(logPath)
	if err != nil {
		// Fallback to stored tail
		var tail string
		db.QueryRow(`SELECT output_tail FROM spawns WHERE id=?`, id).Scan(&tail)
		if tail != "" {
			fmt.Fprintf(os.Stderr, "(log file gone, showing stored tail)\n")
			fmt.Print(tail)
		} else {
			fmt.Fprintf(os.Stderr, "log file not found: %s\n", logPath)
		}
		return
	}
	fmt.Print(string(data))
}

// readTail reads the last n bytes of a file
func readTail(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) <= n {
		return string(data)
	}
	return string(data[len(data)-n:])
}

// serverAPI provides common helpers for talking to the weiran server HTTP API.
type serverAPI struct {
	addr   string
	token  string
	client *http.Client
}

// newServerAPI creates a serverAPI and probes /api/health.
// Returns nil if the server is not reachable or unhealthy.
func newServerAPI() *serverAPI {
	cfg := loadServerConfig()
	addr := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(addr + "/api/health")
	if err != nil {
		return nil
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	return &serverAPI{addr: addr, token: cfg.Token, client: client}
}

// sessionResult is the parsed response from POST /api/sessions.
type sessionResult struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s sessionResult) short() string {
	if len(s.ID) > 8 {
		return s.ID[:8]
	}
	return s.ID
}

// createSession posts to /api/sessions and returns the parsed result.
func (api *serverAPI) createSession(payload map[string]interface{}) (*sessionResult, error) {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", api.addr+"/api/sessions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+api.token)

	resp, err := api.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("server %d: %s", resp.StatusCode, string(respBody))
	}

	var result sessionResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("invalid response: %w", err)
	}
	return &result, nil
}

// waitSession blocks until the session becomes idle or times out.
// Client timeout (11min) > server timeout (10min) to avoid premature disconnect.
func (api *serverAPI) waitSession(sessionID string) error {
	waitClient := &http.Client{Timeout: 11 * time.Minute}
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/sessions/%s/wait?timeout=600s", api.addr, sessionID), nil)
	req.Header.Set("Authorization", "Bearer "+api.token)

	resp, err := waitClient.Do(req)
	if err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("wait returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// handleBareSpawn creates a session without soul files via server API.
// Used for pure-role tasks like code review where persona is noise.
//
// backend (Round 4) is forwarded as the "backend" body field. Empty / "auto"
// lets the server auto-route by model; "cc" pins Claude Code; "codex" pins
// the OpenAI codex JSON-RPC backend; "grok" pins Grok Build headless.
func resolveSpawnModel(model, backend string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "codex", "grok", "kimi", "agy":
		return model, nil
	default:
		return resolveFuzzyModel(model)
	}
}

func handleBareSpawn(name, model, project, task string, wait bool, backend string) {
	api := newServerAPI()
	if api == nil {
		fmt.Fprintf(os.Stderr, "[%s] --bare requires server to be running\n", appName)
		os.Exit(1)
	}

	payload := map[string]interface{}{
		"name":            name,
		"model":           model,
		"initial_message": task,
		"soul_files":      false,
		"category":        "spawn",
	}
	if project != "" {
		payload["project"] = project
	}
	if backend != "" {
		payload["backend"] = backend
	}

	result, err := api.createSession(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] bare spawn failed: %v\n", appName, err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "[%s] bare session created: %s (%s) model=%s\n", appName, result.short(), result.Name, model)

	if wait {
		fmt.Fprintf(os.Stderr, "[%s] waiting for session %s...\n", appName, result.short())
		if err := api.waitSession(result.ID); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] %v\n", appName, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "[%s] session %s completed\n", appName, result.short())
	}

	// Output session ID for caller to use with `weiran session read`
	fmt.Printf("%s\n", result.ID)
}

// delegateSpawnToServer tries to proxy the spawn to weiran server's session API.
// Returns true if delegation succeeded (caller should return), false to fall back to subprocess.
func delegateSpawnToServer(agent *agentDef, task string, wait bool) bool {
	api := newServerAPI()
	if api == nil {
		return false
	}

	sessionName := fmt.Sprintf("spawn-%s-%s", agent.ID, time.Now().Format("0102-1504"))
	result, err := api.createSession(map[string]interface{}{
		"name":            sessionName,
		"project":         agent.Workspace,
		"model":           agent.Model,
		"initial_message": task,
		"category":        "spawn",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] server delegation failed, falling back: %v\n", appName, err)
		return false
	}

	fmt.Fprintf(os.Stderr, "[%s] delegated spawn to server: session %s (%s)\n", appName, result.short(), result.Name)

	// Record in spawns DB for `spawn list` compatibility
	spawnID := recordSpawnStart(agent, task, result.Name, "")

	if wait {
		fmt.Fprintf(os.Stderr, "[%s] waiting for session %s to complete...\n", appName, result.short())
		if err := api.waitSession(result.ID); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] %v\n", appName, err)
			recordSpawnFinish(spawnID, 1, 0, "")
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "[%s] session %s completed\n", appName, result.short())
		recordSpawnFinish(spawnID, 0, 0, "")
	} else {
		fmt.Printf("spawn_ok id=%d agent=%s session=%s server=true\n", spawnID, agent.ID, result.Name)
	}

	return true
}

func parseFloat(s string) float64 {
	v := 0.0
	fmt.Sscanf(s, "%f", &v)
	return v
}

func parseInt(s string) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
