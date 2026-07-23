package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kiyor/soul-cli/pkg/provider"

	_ "modernc.org/sqlite"
)

//go:embed VERSION
var embeddedVersion string

// buildVersion is embedded from the VERSION file at compile time (affects
// the binary content — if you bump VERSION, CDHash changes, which is desired).
// defaultAppName is injected via ldflags (stable per APP_NAME target).
//
// buildCommit / buildDate are intentionally NOT ldflags-injected — they are
// loaded at runtime from an external meta file (see initBuildMeta). This
// keeps the binary byte-stable across commits: signing only changes when
// real code or VERSION change. Without this, every git commit would alter
// the embedded build metadata, change the CDHash, and macOS Local Network
// Privacy would re-prompt for LAN permission on each rebuild.
var (
	buildVersion   = ""
	buildDate      = "unknown"
	buildCommit    = "unknown"
	defaultAppName = "" // injected via ldflags; takes priority over os.Args[0]
)

// toolCLIConfig describes a non-conversational CLI that workers can invoke in
// the shared project environment. Tool CLIs do not implement Backend.
type toolCLIConfig struct {
	Enabled      bool     `json:"enabled"`
	Binary       string   `json:"binary"`
	Capabilities []string `json:"capabilities"`
	DispatchHint string   `json:"dispatch_hint"`
}

var (
	home    = os.Getenv("HOME")
	appName string // derived from os.Args[0] basename, e.g. "weiran", "soul"

	workspace         string // <appHome>/workspace
	agentName         string // defaults to appName, overridable via config.json
	agentAvatarURL    string // optional avatar image URL (from config.json "avatarUrl")
	agentWelcomeImage string // optional full-body welcome image URL (from config.json "welcomeImage")
	userAvatarURL     string // optional user avatar image URL (from config.json "userAvatarUrl")
	claudeBin         = findClaudeBin()
	lockfile          string // /tmp/<appName>.lock
	dbPath            string // <appDir>/sessions.db

	// Claude Code config directory (sessions, projects, settings).
	// Defaults to ~/.claude; override with CLAUDE_CONFIG_DIR env var for instance isolation.
	claudeConfigDir         string
	claudeConfigDirExplicit bool // true when CLAUDE_CONFIG_DIR was explicitly set by user

	// Per-session temp directory, initialized by initSessionDir()
	sessionDir string // /tmp/<agent>-0405-0924/
	promptOut  string // sessionDir/prompt.md

	jiraToken string // read from config.json or JIRA_TOKEN env var

	// ownerName is the human user's display name (e.g. "Kiyor", "Alice").
	// Resolved at startup from (in priority order):
	//   1. config.json "ownerName"
	//   2. <APPNAME>_OWNER_NAME env var
	//   3. USER.md frontmatter "Name:" line
	//   4. fallback: "the user"
	// Used wherever framework code needs to address the user by name in
	// runtime-visible strings (prompts, system reminders, TG context). Never
	// hardcode a specific name — soul-cli is open-source.
	ownerName string

	// agentNick is the AI agent's display name / nickname (e.g. "未然", "Kuro").
	// Distinct from appName (binary code name like "weiran") and agentName
	// (multi-agent ID). Used wherever the agent self-references in prompts /
	// messages so the user sees the personality name, not the binary path.
	// Resolved at startup from (in priority order):
	//   1. config.json "agentNick"
	//   2. <APPNAME>_AGENT_NICK env var
	//   3. IDENTITY.md frontmatter "Name:" line
	//   4. fallback: appName
	agentNick string

	isServerMode    bool   // set to true when running as `weiran server`
	serverPort      int    // port the server is listening on (set in handleServer)
	serverAuthToken string // auth token for server API (set in handleServer)

	// Custom model override: --model provider/model (e.g. zai/glm-5.1, minimax/MiniMax-M2.7)
	// When set, injects provider's baseUrl + apiKey as ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN
	overrideModel string

	// Category override: --category cron|heartbeat|evolve
	// Used with -p to mark one-shot sessions as non-interactive in server_sessions DB.
	overrideCategory string

	// Core/Backend override: --core grok|codex|cc
	// When set, forces the session to use a specific harness backend regardless
	// of model auto-routing. Non-CC backends force -p mode to delegate to server
	// because those backends live in the server process. Empty = auto-route.
	overrideCore string

	// Default model for cron/heartbeat (from config.json "defaultModel")
	defaultModel string
	// Default model for interactive/print modes (from config.json "server.defaultInteractiveModel")
	defaultInteractiveModel string
	// Fallback models for non-interactive modes (from config.json "defaultModelFallbacks")
	defaultModelFallbacks []string

	// Max heartbeat rounds before soul session auto-compact (from config.json "soulSessionMaxRounds")
	// 0 = disabled. Default 30 (~7.5h at 15-min intervals).
	soulSessionMaxRounds int

	// Codex backend config (from config.json "agents.codex.*"). Round 4
	// adds the OpenAI codex app-server JSON-RPC backend as an alternative
	// to the CC stream-json subprocess. All fields optional; codexEnabled
	// defaults to false so existing users see no behavior change.
	codexEnabled           bool              // master switch
	codexBinary            string            // path to codex binary; default "codex" via PATH
	codexModelMap          map[string]string // weiran model name → codex model name (e.g. "opus[1m]" → "gpt-5.1-codex-max")
	codexPermissionProfile string            // default codex permission profile (e.g. "workspaceWrite")
	codexApprovalPolicy    string            // default codex approval policy (e.g. "never")
	codexDispatchHint      string            // optional local routing guidance injected into the supervisor prompt

	// Grok backend config (from config.json "agents.grok.*"). Grok Build
	// runs as a headless one-shot subprocess per user turn. Disabled by
	// default so existing users see no behavior change.
	grokEnabled      bool              // master switch
	grokBinary       string            // path to grok binary; default "grok" via PATH
	grokModelMap     map[string]string // weiran model name → grok model name (e.g. "grok-fast" → "grok-4-fast")
	grokDispatchHint string

	// Kimi CLI headless one-shot backend config.
	kimiEnabled      bool
	kimiBinary       string
	kimiModelMap     map[string]string
	kimiDispatchHint string

	// Antigravity CLI headless one-shot backend config.
	agyEnabled                    bool
	agyBinary                     string
	agyModelMap                   map[string]string
	agyDispatchHint               string
	agyMode                       string
	agyEffort                     string
	agyDangerouslySkipPermissions bool

	// Non-conversational CLIs available to workers in the shared environment.
	toolCLIs map[string]toolCLIConfig

	launchDir  string // original working directory before chdir to workspace
	galContext string // GAL save JSON injected into prompt for resume

	// Skill directories
	skillDirs []string

	// Telegram (read from config.json / <APPNAME>_TG_CHAT_ID)
	tgChatID string

	// Base directory: <APPNAME>_HOME env, then WEIRAN_HOME, then ~/.<appName>
	appHome string

	// Version management
	versionsDir string // <appDir>/.versions/
	appBin      string // ~/.local/bin/<appName>
	maxVersions = 3

	// App data directory: where sessions.db, metrics.jsonl, hooks/, .versions/ live
	appDir string // <appHome>/data/ (sessions.db, metrics, hooks, versions)
	srcDir string // source code directory (for build/update commands)

	// Service health check (from services.json or fallback defaults)
	monitoredServices []serviceEntry

	// Current run mode (cron/heartbeat/interactive etc.), used for metrics recording
	currentMode string

	// replaceSoul enables Id Mode (本我) — use --system-prompt-file (replace CC's
	// native system prompt) instead of --append-system-prompt-file.
	// DEFAULT: true (本我模式). Use --standard to switch to append mode.
	replaceSoul = true

	// Extra projectRoots read from config.json
	extraProjectRoots []string
)

// findClaudeBin locates the claude binary. Priority:
//  1. ~/.local/bin/claude (conventional install location)
//  2. PATH lookup (for npm global installs on Linux: /usr/bin/claude, /usr/local/bin/claude)
//  3. Falls back to ~/.local/bin/claude (will fail gracefully at exec time)
func findClaudeBin() string {
	preferred := filepath.Join(home, ".local", "bin", "claude")
	if _, err := os.Stat(preferred); err == nil {
		return preferred
	}
	// Search PATH for claude binary
	if pathEnv := os.Getenv("PATH"); pathEnv != "" {
		for _, dir := range filepath.SplitList(pathEnv) {
			candidate := filepath.Join(dir, "claude")
			if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
				return candidate
			}
		}
	}
	return preferred // fallback; error will surface when we try to exec
}

// initAppName derives appName from (in priority order):
//  1. ldflags: -X main.defaultAppName=weiran
//  2. AGENT_NAME env var
//  3. os.Args[0] basename
func initAppName() {
	switch {
	case defaultAppName != "":
		appName = defaultAppName
	case os.Getenv("AGENT_NAME") != "":
		appName = os.Getenv("AGENT_NAME")
	case len(os.Args) > 0:
		appName = filepath.Base(os.Args[0])
		appName = strings.TrimSuffix(appName, ".test")
	}
	if appName == "" || appName == "." {
		appName = "soul" // fallback for tests or edge cases
	}

	// Derive early vars that depend on appName
	upperName := strings.ToUpper(strings.ReplaceAll(appName, "-", "_"))
	appHome = envOrDefault(upperName+"_HOME", envOrDefault("WEIRAN_HOME", filepath.Join(home, appName)))
	lockfile = filepath.Join(os.TempDir(), appName+".lock")
	appBin = filepath.Join(home, ".local", "bin", appName)

	// Claude Code config dir isolation: CLAUDE_CONFIG_DIR env → default ~/.claude
	claudeConfigDir = os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeConfigDir != "" {
		claudeConfigDirExplicit = true
	} else {
		claudeConfigDir = filepath.Join(home, ".claude")
	}
}

// initWorkspace initializes workspace path and all derived paths.
// Workspace defaults to <appHome>/workspace.
func initWorkspace() {
	workspace = filepath.Join(appHome, "workspace")
	agentName = appName

	// Telegram chat ID: read from credentials (lowest priority)
	allowFromPath := filepath.Join(appHome, "credentials", "telegram-default-allowFrom.json")
	if afData, err := os.ReadFile(allowFromPath); err == nil {
		var af struct {
			AllowFrom []string `json:"allowFrom"`
		}
		if json.Unmarshal(afData, &af) == nil && len(af.AllowFrom) > 0 {
			tgChatID = af.AllowFrom[0]
		}
	}

	// Read user config from config.json (may override tgChatID/jiraToken/projectRoots)
	loadConfig()

	// App data directory: <appHome>/data/ (XDG-style, independent of source tree)
	// Legacy: workspace/scripts/<appName>/ (auto-migrated)
	appDir = filepath.Join(appHome, "data")
	os.MkdirAll(appDir, 0755)

	// Migrate from legacy source-tree location if data exists there
	legacyDir := filepath.Join(workspace, "scripts", appName)
	migrateAppData(legacyDir, appDir)

	// Source directory: where the CLI's own Go source lives (for build/update/evolve audit).
	// Resolution priority:
	//   1. Env override: <APPNAME>_SRC_DIR (e.g. WEIRAN_SRC_DIR) or generic SOUL_CLI_SRC_DIR
	//   2. Conventional: workspace/scripts/<appName>/ with soul-cli signature files
	//   3. Fallback: directory of the running executable (dev-mode `go run`), same signature check
	//
	// "soul-cli signature" = go.mod + main.go + tasks.go all present. This prevents
	// picking up an unrelated Go project whose go.mod happens to live in a fallback dir
	// (e.g. someone's /tmp/go.mod debug snippet).
	srcDirEnv := strings.ToUpper(appName) + "_SRC_DIR"
	srcDirEnv = strings.ReplaceAll(srcDirEnv, "-", "_")
	isSoulCliSrc := func(dir string) bool {
		for _, f := range []string{"go.mod", "main.go", "tasks.go"} {
			if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
				return false
			}
		}
		return true
	}
	srcDir = ""
	if v := os.Getenv(srcDirEnv); v != "" && isSoulCliSrc(v) {
		srcDir = v
	} else if v := os.Getenv("SOUL_CLI_SRC_DIR"); v != "" && isSoulCliSrc(v) {
		srcDir = v
	} else if c := filepath.Join(workspace, "scripts", appName); isSoulCliSrc(c) {
		srcDir = c
	} else if exe, err := os.Executable(); err == nil {
		c := filepath.Dir(exe)
		if isSoulCliSrc(c) {
			srcDir = c
		}
	}
	// If nothing matched, leave srcDir empty — tasks.go's hasSourceCode() returns false
	// and code-evolution phase is skipped cleanly instead of mis-auditing a random dir.

	// Service monitor list: read from <appDir>/services.json, fallback to hardcoded defaults
	monitoredServices = loadServices()

	// Initialize derived paths
	dbPath = filepath.Join(appDir, "sessions.db")
	versionsDir = filepath.Join(appDir, ".versions")
	initSessionSources()
	skillDirs = []string{
		filepath.Join(claudeConfigDir, "skills"),
		filepath.Join(appHome, "skills"),
		filepath.Join(workspace, "skills"),
	}
	hooksDir = filepath.Join(appDir, "hooks")

	// projectRoots: workspace/projects + scripts always included, extras from config.json
	projectRoots = []string{
		filepath.Join(workspace, "projects"),
		filepath.Join(workspace, "scripts"),
	}
	projectRoots = append(projectRoots, extraProjectRoots...)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// migrateAppData moves data files from legacy source-tree location to the new appDir.
// Only migrates if the destination file does NOT already exist (no overwrite).
func migrateAppData(legacyDir, newDir string) {
	if legacyDir == newDir {
		return
	}
	if _, err := os.Stat(legacyDir); err != nil {
		return // legacy dir doesn't exist, nothing to migrate
	}

	dataFiles := []string{"sessions.db", "metrics.jsonl", "services.json", "config.json"}
	dataDirs := []string{".versions", "hooks"}

	migrated := 0
	for _, name := range dataFiles {
		src := filepath.Join(legacyDir, name)
		dst := filepath.Join(newDir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if _, err := os.Stat(dst); err == nil {
			continue // already exists at destination
		}
		if err := os.Rename(src, dst); err != nil {
			// cross-device: copy + remove
			data, err := os.ReadFile(src)
			if err != nil {
				continue
			}
			info, err := os.Stat(src)
			if err != nil {
				continue
			}
			if os.WriteFile(dst, data, info.Mode()) == nil {
				os.Remove(src)
				migrated++
			}
		} else {
			migrated++
		}
	}

	for _, name := range dataDirs {
		src := filepath.Join(legacyDir, name)
		dst := filepath.Join(newDir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := os.Rename(src, dst); err == nil {
			migrated++
		}
		// skip cross-device dir copy — too complex, user can do manually
	}

	if migrated > 0 {
		fmt.Fprintf(os.Stderr, "[%s] migrated %d data items from %s → %s\n", appName, migrated, legacyDir, newDir)
	}
}

// loadConfig reads user configuration from config.json.
// Config file is located in the app data directory.
// All fields are optional; if unset, values are read from environment variables.
func loadConfig() {
	configPaths := []string{
		filepath.Join(appHome, "data", "config.json"),               // new location
		filepath.Join(workspace, "scripts", appName, "config.json"), // legacy
	}
	// Also check the directory containing the binary (for development)
	if exe, err := os.Executable(); err == nil {
		configPaths = append(configPaths, filepath.Join(filepath.Dir(exe), "config.json"))
	}

	var cfgData []byte
	for _, p := range configPaths {
		if d, err := os.ReadFile(p); err == nil {
			cfgData = d
			break
		}
	}

	type serverBlock struct {
		DefaultInteractiveModel string `json:"defaultInteractiveModel"` // default model for interactive sessions (e.g. "opus[1m]")
	}
	// codexBlock is the agents.codex.* sub-tree of config.json. All fields
	// optional. Round 4 introduces the OpenAI codex app-server backend as an
	// alternative to the CC stream-json subprocess. Disabled by default —
	// users opt in by setting `agents.codex.enabled: true`.
	type codexBlock struct {
		Enabled           bool              `json:"enabled"`            // master switch
		Binary            string            `json:"binary"`             // path to codex binary; default "codex" via PATH
		ModelMap          map[string]string `json:"model_map"`          // weiran model name → codex model name
		PermissionProfile string            `json:"permission_profile"` // codex permission profile (default "workspaceWrite")
		ApprovalPolicy    string            `json:"approval_policy"`    // codex approval policy (default "never")
		DispatchHint      string            `json:"dispatch_hint"`
	}
	type grokBlock struct {
		Enabled      bool              `json:"enabled"`   // master switch
		Binary       string            `json:"binary"`    // path to grok binary; default "grok" via PATH
		ModelMap     map[string]string `json:"model_map"` // weiran model name → grok model name
		DispatchHint string            `json:"dispatch_hint"`
	}
	type kimiBlock struct {
		Enabled      bool              `json:"enabled"`
		Binary       string            `json:"binary"`
		ModelMap     map[string]string `json:"model_map"`
		DispatchHint string            `json:"dispatch_hint"`
	}
	type agyBlock struct {
		Enabled                    bool              `json:"enabled"`
		Binary                     string            `json:"binary"`
		ModelMap                   map[string]string `json:"model_map"`
		DispatchHint               string            `json:"dispatch_hint"`
		Mode                       string            `json:"mode"`
		Effort                     string            `json:"effort"`
		DangerouslySkipPermissions bool              `json:"dangerously_skip_permissions"`
	}
	type agentsBlock struct {
		Codex codexBlock `json:"codex"`
		Grok  grokBlock  `json:"grok"`
		Kimi  kimiBlock  `json:"kimi"`
		Agy   agyBlock   `json:"agy"`
	}
	type appConfig struct {
		JiraToken             string                   `json:"jiraToken"`
		TelegramChatID        string                   `json:"telegramChatID"`
		ProjectRoots          []string                 `json:"projectRoots"`
		AgentName             string                   `json:"agentName"`
		AgentNick             string                   `json:"agentNick"`             // agent's display name / nickname (e.g. "未然")
		OwnerName             string                   `json:"ownerName"`             // human user's display name (e.g. "Alice")
		AvatarURL             string                   `json:"avatarUrl"`             // optional avatar image URL for WebUI
		UserAvatarURL         string                   `json:"userAvatarUrl"`         // optional user avatar image URL for WebUI
		WelcomeImage          string                   `json:"welcomeImage"`          // optional full-body welcome page image URL
		DefaultModel          string                   `json:"defaultModel"`          // default model for cron/heartbeat (e.g. "zai/glm-5.1")
		DefaultModelFallbacks []string                 `json:"defaultModelFallbacks"` // fallback models for non-interactive modes (e.g. ["minimax/MiniMax-M2.7-highspeed"])
		SoulSessionMaxRounds  int                      `json:"soulSessionMaxRounds"`  // 0 = disabled, default 30
		Server                serverBlock              `json:"server"`                // server-side config (shared with server mode)
		Agents                agentsBlock              `json:"agents"`                // per-backend agent config (Round 4: codex)
		Tools                 map[string]toolCLIConfig `json:"tools"`                 // non-conversational CLIs shared by workers
	}
	var cfg appConfig
	if cfgData != nil {
		json.Unmarshal(cfgData, &cfg)
	}

	// jiraToken: config.json > JIRA_TOKEN env var
	if cfg.JiraToken != "" {
		jiraToken = cfg.JiraToken
	}
	if envJira := os.Getenv("JIRA_TOKEN"); envJira != "" {
		jiraToken = envJira
	}

	// telegramChatID: env > config.json > credentials (already read above)
	if cfg.TelegramChatID != "" && tgChatID == "" {
		tgChatID = cfg.TelegramChatID
	}
	upperName := strings.ToUpper(strings.ReplaceAll(appName, "-", "_"))
	if envChat := os.Getenv(upperName + "_TG_CHAT_ID"); envChat != "" {
		tgChatID = envChat
	}
	// backward compat: also check WEIRAN_TG_CHAT_ID
	if tgChatID == "" {
		if envChat := os.Getenv("WEIRAN_TG_CHAT_ID"); envChat != "" {
			tgChatID = envChat
		}
	}

	// agentName: config.json override
	if cfg.AgentName != "" && agentName == appName {
		agentName = cfg.AgentName
	}

	// ownerName: config.json > <APPNAME>_OWNER_NAME env > USER.md frontmatter > "the user"
	if cfg.OwnerName != "" {
		ownerName = cfg.OwnerName
	}
	if envOwner := os.Getenv(upperName + "_OWNER_NAME"); envOwner != "" {
		ownerName = envOwner
	}
	if ownerName == "" {
		ownerName = readNameFromMD(filepath.Join(workspace, "USER.md"))
	}
	if ownerName == "" {
		ownerName = "the user"
	}

	// agentNick: config.json > <APPNAME>_AGENT_NICK env > IDENTITY.md frontmatter > appName
	if cfg.AgentNick != "" {
		agentNick = cfg.AgentNick
	}
	if envNick := os.Getenv(upperName + "_AGENT_NICK"); envNick != "" {
		agentNick = envNick
	}
	if agentNick == "" {
		agentNick = readNameFromMD(filepath.Join(workspace, "IDENTITY.md"))
	}
	if agentNick == "" {
		agentNick = appName
	}

	// avatarUrl: config.json
	if cfg.AvatarURL != "" {
		agentAvatarURL = cfg.AvatarURL
	}
	if cfg.UserAvatarURL != "" {
		userAvatarURL = cfg.UserAvatarURL
	}

	// welcomeImage: config.json
	if cfg.WelcomeImage != "" {
		agentWelcomeImage = cfg.WelcomeImage
	}

	// projectRoots: expand ~ to $HOME
	for _, r := range cfg.ProjectRoots {
		if strings.HasPrefix(r, "~/") {
			r = filepath.Join(home, r[2:])
		}
		extraProjectRoots = append(extraProjectRoots, r)
	}

	// defaultModel: config.json > env var
	if cfg.DefaultModel != "" {
		defaultModel = cfg.DefaultModel
	}
	if envModel := os.Getenv("WEIRAN_DEFAULT_MODEL"); envModel != "" {
		defaultModel = envModel
	}

	// defaultModelFallbacks: config.json
	if len(cfg.DefaultModelFallbacks) > 0 {
		defaultModelFallbacks = cfg.DefaultModelFallbacks
	}

	// defaultInteractiveModel: config.json server.defaultInteractiveModel
	if cfg.Server.DefaultInteractiveModel != "" {
		defaultInteractiveModel = cfg.Server.DefaultInteractiveModel
	}

	// soulSessionMaxRounds: config.json (default 30)
	soulSessionMaxRounds = 30 // default
	if cfg.SoulSessionMaxRounds > 0 {
		soulSessionMaxRounds = cfg.SoulSessionMaxRounds
	}

	// agents.codex.* — Round 4 backend config. Disabled by default; users
	// opt in via agents.codex.enabled in config.json.
	codexEnabled = cfg.Agents.Codex.Enabled
	codexBinary = cfg.Agents.Codex.Binary
	if codexBinary == "" {
		codexBinary = "codex"
	}
	codexModelMap = cfg.Agents.Codex.ModelMap
	codexPermissionProfile = cfg.Agents.Codex.PermissionProfile
	if codexPermissionProfile == "" {
		codexPermissionProfile = codexDefaultPermissionProfile
	}
	codexApprovalPolicy = cfg.Agents.Codex.ApprovalPolicy
	if codexApprovalPolicy == "" {
		codexApprovalPolicy = codexDefaultApprovalPolicy
	}
	codexDispatchHint = cfg.Agents.Codex.DispatchHint

	// agents.grok.* — Grok Build headless backend config. Disabled by
	// default; users opt in via agents.grok.enabled in config.json.
	grokEnabled = cfg.Agents.Grok.Enabled
	grokBinary = cfg.Agents.Grok.Binary
	if grokBinary == "" {
		grokBinary = "grok"
	}
	grokModelMap = cfg.Agents.Grok.ModelMap
	grokDispatchHint = cfg.Agents.Grok.DispatchHint

	// agents.kimi.* — Kimi CLI headless backend config. Disabled by default.
	kimiEnabled = cfg.Agents.Kimi.Enabled
	kimiBinary = cfg.Agents.Kimi.Binary
	if kimiBinary == "" {
		kimiBinary = "kimi"
	}
	kimiModelMap = cfg.Agents.Kimi.ModelMap
	kimiDispatchHint = cfg.Agents.Kimi.DispatchHint

	// agents.agy.* — Antigravity CLI headless backend config.
	agyEnabled = cfg.Agents.Agy.Enabled
	agyBinary = cfg.Agents.Agy.Binary
	if agyBinary == "" {
		agyBinary = "agy"
	}
	agyModelMap = cfg.Agents.Agy.ModelMap
	agyDispatchHint = cfg.Agents.Agy.DispatchHint
	agyMode = cfg.Agents.Agy.Mode
	if agyMode == "" {
		agyMode = "plan"
	}
	agyEffort = cfg.Agents.Agy.Effort
	if agyEffort == "" {
		agyEffort = "low"
	}
	agyDangerouslySkipPermissions = cfg.Agents.Agy.DangerouslySkipPermissions

	toolCLIs = cfg.Tools
}

// readNameFromMD scans the given markdown file's first ~30 lines for a
// "Name:" or "**Name:**" line and returns the value. Used to discover
// ownerName from USER.md and agentNick from IDENTITY.md. Returns empty
// string if not found or file missing.
func readNameFromMD(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(raw), "\n")
	limit := 30
	if len(lines) < limit {
		limit = len(lines)
	}
	for _, line := range lines[:limit] {
		l := strings.TrimSpace(line)
		// strip leading list markers / bold
		l = strings.TrimPrefix(l, "- ")
		l = strings.TrimPrefix(l, "* ")
		l = strings.TrimPrefix(l, "**")
		// match "Name:" or "Name**:" (markdown bold close mid-key)
		for _, prefix := range []string{"Name:", "Name**:"} {
			if strings.HasPrefix(l, prefix) {
				v := strings.TrimSpace(strings.TrimPrefix(l, prefix))
				v = strings.TrimPrefix(v, "**")
				v = strings.TrimSpace(v)
				// stop at first parenthesis or comma (avoid "Alice (alias)")
				if i := strings.IndexAny(v, "(,"); i >= 0 {
					v = strings.TrimSpace(v[:i])
				}
				return v
			}
		}
	}
	return ""
}

// loadBuildMeta reads buildCommit / buildDate from an external meta file next
// to the installed binary. Format is simple `key=value` lines. If the file is
// missing or malformed, buildCommit / buildDate stay "unknown".
//
// Why external instead of ldflags: see the comment on buildCommit/buildDate
// declaration. Summary: keeps the binary byte-stable across commits so
// macOS Local Network Privacy (TCC) doesn't re-prompt per rebuild.
func loadBuildMeta() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	// resolve symlinks so the meta sits next to the real file
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	metaPath := exe + ".meta"
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "commit":
			if v != "" {
				buildCommit = v
			}
		case "date":
			if v != "" {
				buildDate = v
			}
		}
	}
}

func init() {
	traceT0 := time.Now()
	if buildVersion == "" {
		buildVersion = strings.TrimSpace(embeddedVersion)
	}
	loadBuildMeta()
	// Capture original working directory before any chdir
	launchDir, _ = os.Getwd()
	initAppName()
	tracePoint("init:appname", traceT0)
	initWorkspace()
	tracePoint("init:workspace", traceT0)
	initProviderPkg()
	tracePoint("init:provider-pkg", traceT0)
}

// initProviderPkg wires pkg/provider's injection points to main-package state.
// Called once at startup so the provider subpackages see the same appName
// label and the same server address (token + port) main itself uses.
func initProviderPkg() {
	provider.AppName = appName
	provider.LoadServerAddr = func() provider.ServerAddr {
		c := loadServerConfig()
		return provider.ServerAddr{
			Token: c.Token,
			Host:  c.Host,
			Port:  c.Port,
		}
	}
}

// tracePoint is a zero-cost timing probe when WEIRAN_HOOK_TRACE is unset.
// When set, prints cumulative elapsed time from the given start to stderr,
// tagged with the label. Used only for ad-hoc profiling; never compiled out.
func tracePoint(label string, start time.Time) {
	if os.Getenv("WEIRAN_HOOK_TRACE") != "1" {
		return
	}
	fmt.Fprintf(os.Stderr, "[trace] %s=%s\n", label, time.Since(start))
}

// initSessionDir creates /tmp/<agent>-MMDD-HHMM/ as the temp directory for this session
func initSessionDir() {
	name := agentName + "-" + time.Now().Format("0102-1504")
	sessionDir = filepath.Join(os.TempDir(), name)
	os.MkdirAll(sessionDir, 0700)
	promptOut = filepath.Join(sessionDir, "prompt.md")
}

// sessionTmp returns the file path under the session temp directory
func sessionTmp(name string) string {
	return filepath.Join(sessionDir, name)
}

// ── main ──

func helpText() string {
	upper := strings.ToUpper(strings.ReplaceAll(appName, "-", "_"))
	text := `{{NAME}} — Claude Code launcher with soul prompt

Usage:
  {{NAME}}                       interactive session (Id Mode 本我, default)
  {{NAME}} --standard            Standard mode — append soul to CC's native system prompt
  {{NAME}} -p "task"             one-shot task, exits when done
  {{NAME}} --standard -p "task"  one-shot task in Standard mode
  {{NAME}} --model glm            fuzzy match: "glm"→zai/glm-5.1, "highspeed"→minimax, "opus"→native
  {{NAME}} --model zai/glm-5.1    fully qualified (see '{{NAME}} models')
  {{NAME}} models                 list all available models (native + provider/model)
  {{NAME}} -r                    resume session (opens TUI picker if no ID given)
  {{NAME}} -r <session-id>       resume specific session
  {{NAME}} --cron                memory consolidation mode (for scheduled tasks)
  {{NAME}} --heartbeat           heartbeat patrol mode
  {{NAME}} --evolve              self-evolution mode (daily cron, improves if inspired, skips otherwise)
  {{NAME}} server                 HTTP API server (persistent Claude Code sessions)

Subcommands:
  {{NAME}} build                 safe build: backup current version -> go build -> verify -> auto-rollback on failure
  {{NAME}} versions              list historical versions
  {{NAME}} rollback [N]          rollback to Nth version (default 1 = previous)
  {{NAME}} update                self-update: git pull -> {{NAME}} build (safe build)
  {{NAME}} status                quick health check (without launching claude)
  {{NAME}} models                list available models grouped by provider (native + custom)
  {{NAME}} doctor                deep diagnostics (claude version, processes, DB, disk, model endpoints, metrics anomalies)
  {{NAME}} doctor cron           audit crontab: binary path, schedule sanity, log health, evolve-probe readiness
  {{NAME}} prompt                 print the full assembled system prompt to stdout
  {{NAME}} lint                   validate md file formats (topics, skills, CLAUDE.md)
  {{NAME}} diff                  show soul/memory file changes since last commit
  {{NAME}} config                show current config (workspace, agent, paths)
  {{NAME}} log [N]               view daily notes (N=day offset, default today, 1=yesterday)
  {{NAME}} server                 start HTTP API server for persistent Claude Code sessions
  {{NAME}} server --port 8080    custom port (default 9090)
  {{NAME}} server --host 0.0.0.0 expose to network (default 127.0.0.1)
  {{NAME}} server --token SECRET  set auth token (or use WEIRAN_SERVER_TOKEN env / config.json)
  {{NAME}} token                 print fresh Claude OAuth access_token (for SSH shells; IPCs to server)
  {{NAME}} netprobe              one-shot LAN health sweep (parallel HTTP/TCP probes; one approval covers all)
  {{NAME}} netprobe --only vpn   filter categories (service/gpu/vpn/infra/extra); --json / --list / --timeout 2s / --extra <url>
  {{NAME}} clean                 clean old session temp directories under /tmp
  {{NAME}} sessions [keyword]    search sessions (fuzzy match on title/content/project)
  {{NAME}} ss [keyword]          same as above (shorthand)
  {{NAME}} spawn <agent> "task"   dispatch task to another agent (async by default)
  {{NAME}} spawn <agent> "task" --wait  dispatch and wait for completion
  {{NAME}} spawn --self main "task" [--model opus]  spawn with own soul
  {{NAME}} spawn list            show running/recent spawn processes
  {{NAME}} evolve-probe -f <feedback> -s <scenario> [--mode with|without|both]
                                  run a thought-experiment probe against a feedback rule
  {{NAME}} evolve-probe -f <feedback> --list   list scenarios for a feedback
  {{NAME}} init                  first-run setup wizard (creates workspace + soul files)
  {{NAME}} init --yes            use defaults, no interactive prompts
  {{NAME}} init --force          overwrite existing files
  {{NAME}} init --archetype companion  use a personality archetype (companion/engineer/steward/mentor)
  {{NAME}} init --name X --role Y --personality Z --owner W --tz TZ  flag-driven (AI-friendly)
  {{NAME}} new                   reset all Telegram direct sessions (start new conversation)
  {{NAME}} notify [--dry-run] <message>      send Telegram text message to user (supports Markdown)
  {{NAME}} notify-photo [--dry-run] <URL> [caption]  send Telegram photo to user
  {{NAME}} notify-voice [--dry-run] <ogg-file> [caption]  send Telegram voice (OGG/Opus)
  {{NAME}} notify-album [--dry-run] [--caption "..."] <URL1> <URL2> [...]  send media group (2-10 photos)
  {{NAME}} notify-edit [--dry-run] <message_id> <new-text>  edit a previously sent message in place
  {{NAME}} notify-keyboard [--dry-run] [--buttons "L=cb,L2=https://...,/n,L3=cb"] <text>
                                            send message with inline keyboard, prints message_id
  {{NAME}} notify-action <action>            send chat action (typing/upload_photo/record_voice/...)
  {{NAME}} db recall             view sessions pending scan and instructions
  {{NAME}} db pending            JSON output of sessions pending scan
  {{NAME}} db summarized         JSON output of sessions with summaries
  {{NAME}} db save '<json>'      save a single summary: {"path":"...","summary":"..."}
  {{NAME}} db save-batch '<json>' batch save summaries (also supports stdin: echo '[...]' | {{NAME}} db save-batch -)
  {{NAME}} db list               view last 20 records
  {{NAME}} db stats              show session count and summary count
  {{NAME}} db search <keyword>   search session summaries
  {{NAME}} db gc                 clean up DB records for deleted sessions
  {{NAME}} db patterns [-j]      list patterns (-j for JSON output)
  {{NAME}} db pattern-save '<json>'  save/update pattern: {"name":"...","description":"...","example":"...","source":"..."}
  {{NAME}} db pattern-save-batch batch save patterns (supports stdin)
  {{NAME}} db feedback '<json>'  record pattern feedback: {"pattern":"...","outcome":"success|failure|correction","note":"..."}
  {{NAME}} db cultivate [--dry-run]  run skill cultivation (auto-generates SKILL.md when threshold met)
  {{NAME}} db pattern-reject <name>  manually reject a pattern

Hooks:
  Post-hooks run automatically after claude finishes in cron/heartbeat mode:
  - Built-in: import summaries.json from session temp directory into DB
  - Custom: <appDir>/hooks/{cron,heartbeat}.d/*.sh

Environment variables:
  JIRA_TOKEN                Jira agent token (can also be set in config.json)
  {{UPPER}}_TG_CHAT_ID     Telegram chat ID (can also be set in config.json)
  {{UPPER}}_HOME            Base directory (default: ~/.<appName>)

Examples:
  {{NAME}}                              # start interactive session
  {{NAME}} -p "check my Jira backlog"   # one-shot task
  {{NAME}} status                       # quick health check
  {{NAME}} clean                        # clean temp files
  {{NAME}} notify "cron found anomaly"  # send notification to user
  {{NAME}} db recall                    # view sessions needing recall
  {{NAME}} db stats                     # check database status
  {{NAME}} --version                    # show version info
`
	text = strings.ReplaceAll(text, "{{NAME}}", appName)
	text = strings.ReplaceAll(text, "{{UPPER}}", upper)
	return text
}

func main() {
	traceMain := time.Now()
	tracePoint("main:enter", traceMain)
	// help / version
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--help", "-h", "help":
			fmt.Print(helpText())
			return
		case "--version", "-v", "version":
			fmt.Printf("%s %s (%s) built %s\n", appName, buildVersion, buildCommit, buildDate)
			return
		}
	}

	if jiraToken != "" {
		os.Setenv("JIRA_TOKEN", jiraToken)
	}

	mode, printPrompt, extra := parseArgs(os.Args[1:])
	currentMode = mode

	switch mode {
	case "db":
		handleDB(extra)
		return
	case "sessions":
		handleSessions(extra)
		return
	case "notify":
		dryRun := false
		var msgParts []string
		for _, a := range extra {
			if a == "--dry-run" {
				dryRun = true
			} else {
				msgParts = append(msgParts, a)
			}
		}
		msg := strings.Join(msgParts, " ")
		if msg == "" {
			fmt.Fprintf(os.Stderr, "usage: %s notify [--dry-run] <message>\n", appName)
			os.Exit(1)
		}
		if dryRun {
			fmt.Printf("[dry-run] would send TG: %s\n", msg)
		} else {
			sendTelegram(msg)
		}
		return
	case "notify-photo":
		dryRun := false
		var photoParts []string
		for _, a := range extra {
			if a == "--dry-run" {
				dryRun = true
			} else {
				photoParts = append(photoParts, a)
			}
		}
		if len(photoParts) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s notify-photo [--dry-run] <image-URL> [caption]\n", appName)
			os.Exit(1)
		}
		caption := ""
		if len(photoParts) > 1 {
			caption = strings.Join(photoParts[1:], " ")
		}
		if dryRun {
			fmt.Printf("[dry-run] would send TG photo: %s (caption: %s)\n", photoParts[0], caption)
		} else {
			sendTelegramPhoto(photoParts[0], caption)
		}
		return
	case "notify-voice":
		dryRun := false
		var voiceParts []string
		for _, a := range extra {
			if a == "--dry-run" {
				dryRun = true
			} else {
				voiceParts = append(voiceParts, a)
			}
		}
		if len(voiceParts) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s notify-voice [--dry-run] <ogg-file> [caption]\n", appName)
			fmt.Fprintf(os.Stderr, "  TG voice messages must be OGG/Opus mono. Convert WAV with:\n")
			fmt.Fprintf(os.Stderr, "    ffmpeg -i in.wav -c:a libopus -b:a 64k out.ogg\n")
			os.Exit(1)
		}
		caption := ""
		if len(voiceParts) > 1 {
			caption = strings.Join(voiceParts[1:], " ")
		}
		if dryRun {
			fmt.Printf("[dry-run] would send TG voice: %s (caption: %s)\n", voiceParts[0], caption)
		} else {
			sendTelegramVoice(voiceParts[0], caption)
		}
		return
	case "notify-album":
		dryRun := false
		caption := ""
		var urls []string
		for i := 0; i < len(extra); i++ {
			a := extra[i]
			switch {
			case a == "--dry-run":
				dryRun = true
			case a == "--caption" && i+1 < len(extra):
				caption = extra[i+1]
				i++
			case strings.HasPrefix(a, "--caption="):
				caption = strings.TrimPrefix(a, "--caption=")
			default:
				urls = append(urls, a)
			}
		}
		if len(urls) < 2 {
			fmt.Fprintf(os.Stderr, "usage: %s notify-album [--dry-run] [--caption \"...\"] <URL1> <URL2> [...] (2-10 photos)\n", appName)
			os.Exit(1)
		}
		if dryRun {
			fmt.Printf("[dry-run] would send TG album: %d photos (caption: %s)\n", len(urls), caption)
		} else {
			sendTelegramAlbum(urls, caption)
		}
		return
	case "notify-edit":
		dryRun := false
		var rest []string
		for _, a := range extra {
			if a == "--dry-run" {
				dryRun = true
			} else {
				rest = append(rest, a)
			}
		}
		if len(rest) < 2 {
			fmt.Fprintf(os.Stderr, "usage: %s notify-edit [--dry-run] <message_id> <new-text>\n", appName)
			os.Exit(1)
		}
		var mid int64
		if _, err := fmt.Sscanf(rest[0], "%d", &mid); err != nil || mid <= 0 {
			fmt.Fprintf(os.Stderr, "invalid message_id: %s\n", rest[0])
			os.Exit(1)
		}
		newText := strings.Join(rest[1:], " ")
		if dryRun {
			fmt.Printf("[dry-run] would edit TG msg %d → %s\n", mid, newText)
		} else {
			editTelegramMessage(mid, newText)
		}
		return
	case "notify-keyboard":
		dryRun := false
		buttonsSpec := ""
		var rest []string
		for i := 0; i < len(extra); i++ {
			a := extra[i]
			switch {
			case a == "--dry-run":
				dryRun = true
			case a == "--buttons" && i+1 < len(extra):
				buttonsSpec = extra[i+1]
				i++
			case strings.HasPrefix(a, "--buttons="):
				buttonsSpec = strings.TrimPrefix(a, "--buttons=")
			default:
				rest = append(rest, a)
			}
		}
		if len(rest) == 0 {
			fmt.Fprintf(os.Stderr, "usage: %s notify-keyboard [--buttons \"L=cb,L2=https://...,/n,L3=cb\"] <text>\n", appName)
			os.Exit(1)
		}
		text := strings.Join(rest, " ")
		rows := parseButtonsArg(buttonsSpec)
		if len(rows) == 0 {
			fmt.Fprintf(os.Stderr, "no buttons parsed from --buttons; need at least one\n")
			os.Exit(1)
		}
		if dryRun {
			fmt.Printf("[dry-run] would send TG keyboard: %s (rows=%d)\n", text, len(rows))
		} else {
			sendTelegramKeyboard(text, rows)
		}
		return
	case "notify-action":
		if len(extra) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s notify-action <action> (typing|upload_photo|record_voice|upload_voice|upload_document|choose_sticker|find_location|upload_video|record_video)\n", appName)
			os.Exit(1)
		}
		if err := trySendTelegramChatAction(extra[0]); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] %v\n", appName, err)
			os.Exit(1)
		}
		fmt.Println("action sent")
		return
	case "status":
		handleStatus()
		return
	case "models":
		handleModels()
		return
	case "log":
		handleLog(extra)
		return
	case "clean":
		handleClean()
		return
	case "build":
		handleBuild()
		return
	case "versions":
		handleVersions()
		return
	case "rollback":
		n := 1
		if len(extra) > 0 {
			if v, err := strconv.Atoi(extra[0]); err == nil && v >= 1 && v <= maxVersions {
				n = v
			}
		}
		handleRollback(n)
		return
	case "update":
		handleUpdate()
		return
	case "config":
		handleConfig()
		return
	case "doctor":
		if len(extra) > 0 && extra[0] == "cron" {
			handleDoctorCron()
		} else {
			handleDoctor()
		}
		return
	case "diff":
		handleDiff()
		return
	case "prompt":
		handlePrompt()
		return
	case "lint":
		handleLint()
		return
	case "new":
		handleNew()
		return
	case "spawn":
		handleSpawn(extra)
		return
	case "evolve-probe":
		handleProbe(extra)
		return
	case "init":
		handleInit(extra)
		return
	case "session":
		handleSessionIPC(extra)
		return
	case "webhook":
		runWebhookCmd(extra)
		return
	case "server":
		handleServer(extra)
		return
	case "token":
		handleToken(extra)
		return
	case "tool-hook":
		tracePoint("main:dispatch_tool_hook", traceMain)
		handleToolHook(extra)
		return
	case "netprobe":
		handleNetProbe(extra)
		return
	case "say":
		handleSay(extra)
		return
	}

	initSessionDir()
	result := buildPrompt()
	writePrompt(result)

	// Apply default models based on mode:
	// - Automated modes (cron/heartbeat/evolve) use defaultModel (cheaper/faster provider)
	// - Interactive/print modes use defaultInteractiveModel (from server.defaultInteractiveModel)
	if overrideModel == "" {
		if defaultModel != "" && (mode == "cron" || mode == "heartbeat" || mode == "evolve") {
			overrideModel = defaultModel
			fmt.Fprintf(os.Stderr, "[%s] using defaultModel: %s\n", appName, defaultModel)
		} else if defaultInteractiveModel != "" && (mode == "" || mode == "interactive" || mode == "print") {
			overrideModel = defaultInteractiveModel
			fmt.Fprintf(os.Stderr, "[%s] using defaultInteractiveModel: %s\n", appName, defaultInteractiveModel)
		}
	}

	promptFlag := "--append-system-prompt-file"
	if replaceSoul {
		promptFlag = "--system-prompt-file"
	} else {
		fmt.Fprintln(os.Stderr, "["+appName+"] Standard mode (--append-system-prompt-file)")
	}
	base := []string{promptFlag, promptOut, "--dangerously-skip-permissions"}

	// Inject --model flag for Claude Code when custom model is specified
	if overrideModel != "" {
		base = append(base, "--model", providerModelName(overrideModel))
	}

	switch mode {
	case "interactive":
		// -c (continue) / -r (resume) resumes an existing session, skip --name
		isResume := false
		hasResumeID := false
		for i, a := range extra {
			if a == "-c" || a == "--continue" || a == "-r" || a == "--resume" {
				isResume = true
				// check if next arg looks like a session ID (not another flag)
				if i+1 < len(extra) && !strings.HasPrefix(extra[i+1], "-") {
					hasResumeID = true
				}
				break
			}
		}
		// bare -r without session ID: open TUI to pick a session
		if isResume && !hasResumeID {
			// strip resume flags from extra; the rest (e.g. --chrome) is passed through
			var passthrough []string
			for _, a := range extra {
				if a == "-c" || a == "--continue" || a == "-r" || a == "--resume" {
					continue
				}
				passthrough = append(passthrough, a)
			}
			sessions := scanAllSessions()
			sort.Slice(sessions, func(i, j int) bool {
				return sessions[i].ModTime.After(sessions[j].ModTime)
			})
			chosen := runSessionsTUI(sessions)
			if chosen == nil {
				fmt.Fprintln(os.Stderr, "["+appName+"] no session selected")
				return
			}
			// resume chosen session with soul prompt + passthrough flags
			args := append(base, "--resume", chosen.ID)
			args = append(args, passthrough...)
			execClaude(args)
			return
		}
		args := base
		if !isResume {
			sessionName := agentName + "-" + time.Now().Format("0102-1504")
			args = append(args, "--name", sessionName)
		}
		args = append(args, extra...)
		execClaude(args)

	case "print":
		// Force delegation to server when --core selects a non-CC backend
		// (alternate backends only run inside the server) or --category is set
		// (so the session gets proper category tagging in server_sessions DB).
		if overrideCore == "codex" || overrideCore == "grok" || overrideCore == "kimi" || overrideCore == "agy" {
			category := overrideCategory
			if category == "" {
				category = CategoryInteractive
			}
			if !delegatePrintToServer(printPrompt, category) {
				fmt.Fprintf(os.Stderr, "[%s] --core %s requires running server (start with `%s server`)\n", appName, overrideCore, appName)
				os.Exit(1)
			}
			return
		}
		if overrideCategory != "" && delegatePrintToServer(printPrompt, overrideCategory) {
			return
		}
		args := append(base, "-p", printPrompt)
		args = append(args, extra...)
		execClaude(args)

	case "cron":
		// If server is running, delegate to it so the session gets proper "cron" category.
		if delegatePrintToServer(cronTask(), CategoryCron) {
			// Post-delegate: FTS reindex + hooks still run locally.
			if added, skipped, err := indexDailyNotes(); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] fts reindex failed: %v\n", appName, err)
			} else if added > 0 {
				fmt.Fprintf(os.Stderr, "[%s] fts reindex: +%d daily notes (%d unchanged)\n", appName, added, skipped)
			}
			runHooks("cron")
			os.Exit(0)
		}

		mustLock()
		defer releaseLock()

		// Pre-flight check
		if warnings := preflight(); len(warnings) > 0 {
			for _, w := range warnings {
				fmt.Fprintf(os.Stderr, "["+appName+"] ⚠ preflight: %s\n", w)
			}
		}

		isWeekly := time.Now().Weekday() == time.Sunday
		if isWeekly {
			// Phase 1: haiku pre-scan, generate summaries
			fmt.Fprint(os.Stderr, "["+appName+"] Sunday deep review — phase 1: haiku pre-scan\n")
			scanTask := weeklyPreScan()
			scanArgs := append(base, "--model", "haiku", "--no-session-persistence", "-p", scanTask)
			if code := runClaude(scanArgs); code != 0 {
				fmt.Fprintf(os.Stderr, "[%s] haiku pre-scan failed (exit %d), skipping deep review, running normal cron\n", appName, code)
			} else {
				fmt.Fprint(os.Stderr, "["+appName+"] phase 1 complete, entering phase 2: opus deep review\n")
			}
		}

		task := cronTask()
		beforeCron := time.Now()

		// P0: Log previous crash (metrics only, no prompt injection — cron is stateless)
		if crash := getLastCrashInfo("cron"); crash != nil {
			fmt.Fprintf(os.Stderr, "[%s] previous cron crashed (exit %d at %s): %s\n",
				appName, crash.ExitCode, crash.Timestamp, crash.Error)
			alertOnRepeatedCrash("cron")
		}

		args := append(base, "-p", task)
		args = append(args, extra...)
		exitCode := runClaude(args)

		// Track session ID for crash recovery metrics
		if newSID := detectNewSession(beforeCron); newSID != "" {
			lastClaudeSessionID = newSID
		}

		// Refresh FTS5 index for daily notes after memory consolidation.
		// Best-effort: failure here never blocks cron completion.
		if added, skipped, err := indexDailyNotes(); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] fts reindex failed: %v\n", appName, err)
		} else if added > 0 {
			fmt.Fprintf(os.Stderr, "[%s] fts reindex: +%d daily notes (%d unchanged)\n", appName, added, skipped)
		}

		runHooks("cron")
		os.Exit(exitCode)

	case "heartbeat":
		// If server is running, delegate to /api/wake instead of spawning a subprocess.
		// This ensures heartbeat sessions get proper category in server_sessions DB.
		if delegateToServer("heartbeat") {
			os.Exit(0)
		}

		mustLock()
		defer releaseLock()

		// Pre-flight check
		if warnings := preflight(); len(warnings) > 0 {
			for _, w := range warnings {
				fmt.Fprintf(os.Stderr, "["+appName+"] ⚠ preflight: %s\n", w)
			}
		}

		// Expire stale soul sessions (>24h inactive)
		endStaleSoulSessions()

		hb := heartbeatTask()
		beforeRun := time.Now()

		// P0: Log previous crash (metrics only, no prompt injection — heartbeat is stateless)
		if crash := getLastCrashInfo("heartbeat"); crash != nil {
			fmt.Fprintf(os.Stderr, "[%s] previous heartbeat crashed (exit %d at %s): %s\n",
				appName, crash.ExitCode, crash.Timestamp, crash.Error)
			alertOnRepeatedCrash("heartbeat")
		}

		// Check for active soul session to resume
		if claudeSID := getActiveSoulSession(agentName); claudeSID != "" {
			fmt.Fprintf(os.Stderr, "["+appName+"] resuming soul session (claude %s)\n", shortID(claudeSID))
			lastClaudeSessionID = claudeSID
			args := append(base, "--resume", claudeSID, "-p", hb)
			args = append(args, extra...)
			exitCode := runClaude(args)
			touchSoulSession(agentName)
			updateSoulSessionBudget(agentName)
			runHooks("heartbeat")
			os.Exit(exitCode)
		}

		// No active session — create new daily soul session
		soulID := createSoulSession(agentName, "daily", 2.0)
		args := append(base, "-p", hb)
		args = append(args, extra...)
		exitCode := runClaude(args)

		// Detect and link the Claude session ID created by this run
		if soulID > 0 {
			if newSID := detectNewSession(beforeRun); newSID != "" {
				lastClaudeSessionID = newSID
				linkSoulSession(soulID, newSID)
			}
		}

		runHooks("heartbeat")
		os.Exit(exitCode)

	case "evolve":
		// If server is running, delegate to it so the session gets proper "evolve" category.
		if delegatePrintToServer(evolveTask(), CategoryEvolve) {
			runHooks("evolve")
			os.Exit(0)
		}

		mustLock()
		defer releaseLock()

		// Pre-flight check
		if warnings := preflight(); len(warnings) > 0 {
			for _, w := range warnings {
				fmt.Fprintf(os.Stderr, "["+appName+"] ⚠ preflight: %s\n", w)
			}
		}

		ev := evolveTask()
		args := append(base, "-p", ev)
		args = append(args, extra...)
		exitCode := runClaude(args)
		runHooks("evolve")
		os.Exit(exitCode)
	}
}

// ── argument parsing ──

// delegateToServer tries to delegate a heartbeat/cron/evolve run to the running server.
// Returns true if the server handled it (caller should exit), false if server is not running.
func delegateToServer(mode string) bool {
	cfg := loadServerConfig()
	addr := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)

	var endpoint, reason string
	switch mode {
	case "heartbeat":
		endpoint = "/api/wake"
		reason = "cron-heartbeat"
	default:
		return false
	}

	payload := map[string]string{"text": reason}
	if defaultModel != "" {
		payload["model"] = defaultModel
	}
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(addr+endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] server not reachable, falling back to subprocess mode\n", appName)
		return false
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		fmt.Fprintf(os.Stderr, "[%s] delegated %s to server: %s\n", appName, mode, string(respBody))
		return true
	}
	if resp.StatusCode == http.StatusConflict {
		// Already running — that's fine, skip this cycle
		fmt.Fprintf(os.Stderr, "[%s] server: %s session already running, skipping\n", appName, mode)
		return true
	}

	fmt.Fprintf(os.Stderr, "[%s] server returned %d for %s, falling back to subprocess\n", appName, resp.StatusCode, mode)
	return false
}

// delegatePrintToServer sends a -p task to the running server with a specific category.
// Returns true if the server handled it, false if server is not running.
func delegatePrintToServer(prompt, category string) bool {
	cfg := loadServerConfig()
	addr := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)

	name := fmt.Sprintf("%s-%s-%s", agentName, category, time.Now().Format("0102-1504"))
	// Field name discipline:
	//   "soul_files"   = whether SOUL.md / memory / skills are injected at all
	//                    (true = enabled; we always want enabled here)
	//   "replace_soul" = how the soul prompt is layered:
	//                    true  → 本我 mode (replace built-in system prompt /
	//                             baseInstructions on codex)
	//                    false → standard mode (append-only / developer
	//                             instructions on codex; --standard CLI flag)
	// Historical note: a prior version sent `soul_files: replaceSoul`,
	// which conflated the two and made --standard from CLI a silent no-op
	// (server fell back to cfg.DefaultReplaceSoul instead). Both keys are
	// now sent explicitly so CLI intent reaches the session struct.
	payload := map[string]interface{}{
		"name":            name,
		"category":        category,
		"initial_message": prompt,
		"soul_files":      true,
		"replace_soul":    replaceSoul,
	}
	if overrideModel != "" {
		payload["model"] = overrideModel
	}
	// --core flag → backend hint. "grok" / "codex" / "cc" map directly; "auto" / ""
	// omit the field so server-side resolveBackendKind can auto-route.
	switch overrideCore {
	case "grok":
		payload["backend"] = "grok"
	case "kimi":
		payload["backend"] = "kimi"
	case "agy":
		payload["backend"] = "agy"
	case "codex":
		payload["backend"] = "codex"
	case "cc":
		payload["backend"] = "cc"
	}
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("POST", addr+"/api/sessions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] server not reachable, falling back to direct exec\n", appName)
		return false
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		fmt.Fprintf(os.Stderr, "[%s] delegated -p to server (category=%s): %s\n", appName, category, string(respBody))
		return true
	}

	fmt.Fprintf(os.Stderr, "[%s] server returned %d for -p delegation, falling back to direct exec\n", appName, resp.StatusCode)
	return false
}

func parseArgs(args []string) (mode, printPrompt string, extra []string) {
	mode = "interactive"
	// Defer --model fuzzy resolution until after the full pass so that
	// --core (which may appear before or after --model) can suppress fuzzy
	// matching for codex-side model names like "gpt-5.5" that don't exist
	// in any provider's model list.
	var rawModel string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "db":
			mode = "db"
			extra = args[i+1:]
			return
		case "sessions", "ss":
			mode = "sessions"
			extra = args[i+1:]
			return
		case "notify", "notify-photo", "notify-voice", "notify-album", "notify-edit", "notify-keyboard", "notify-action":
			mode = args[i]
			extra = args[i+1:]
			return
		case "status":
			mode = "status"
			return
		case "models":
			mode = "models"
			return
		case "log":
			mode = "log"
			extra = args[i+1:]
			return
		case "clean":
			mode = "clean"
			return
		case "build":
			mode = "build"
			extra = args[i+1:]
			return
		case "versions":
			mode = "versions"
			return
		case "rollback":
			mode = "rollback"
			extra = args[i+1:]
			return
		case "update":
			mode = "update"
			return
		case "config":
			mode = "config"
			return
		case "doctor":
			mode = "doctor"
			extra = args[i+1:]
			return
		case "diff":
			mode = "diff"
			return
		case "prompt":
			mode = "prompt"
			return
		case "lint":
			mode = "lint"
			return
		case "new":
			mode = "new"
			return
		case "spawn":
			mode = "spawn"
			extra = args[i+1:]
			return
		case "evolve-probe":
			mode = "evolve-probe"
			extra = args[i+1:]
			return
		case "init":
			mode = "init"
			extra = args[i+1:]
			return
		case "session":
			mode = "session"
			extra = args[i+1:]
			return
		case "webhook":
			mode = "webhook"
			extra = args[i+1:]
			return
		case "server":
			mode = "server"
			extra = args[i+1:]
			return
		case "token":
			mode = "token"
			extra = args[i+1:]
			return
		case "tool-hook":
			mode = "tool-hook"
			extra = args[i+1:]
			return
		case "netprobe":
			mode = "netprobe"
			extra = args[i+1:]
			return
		case "say":
			mode = "say"
			extra = args[i+1:]
			return
		case "--cron":
			mode = "cron"
		case "--heartbeat":
			mode = "heartbeat"
		case "--evolve":
			mode = "evolve"
		case "--id", "--soul":
			// already default, no-op for backward compat
			replaceSoul = true
		case "--standard":
			// Standard mode: append to CC's native system prompt instead of replacing
			replaceSoul = false
		case "--model":
			// Custom model: supports fuzzy matching (e.g. "glm", "highspeed", "minimax", "opus")
			// Fuzzy resolution deferred until end of loop so --core can suppress it.
			if i+1 < len(args) {
				rawModel = args[i+1]
				i++
			}
		case "--category":
			if i+1 < len(args) {
				overrideCategory = args[i+1]
				i++
			}
		case "--core":
			// Backend override. Non-CC backends force -p mode to delegate to
			// server with backend hint because they only run there.
			if i+1 < len(args) {
				v := strings.ToLower(strings.TrimSpace(args[i+1]))
				switch v {
				case "grok", "kimi", "agy", "codex", "cc", "auto", "":
					overrideCore = v
				default:
					fmt.Fprintf(os.Stderr, "[%s] unknown --core %q (expected grok|kimi|agy|codex|cc|auto)\n", appName, args[i+1])
					os.Exit(1)
				}
				i++
			}
		case "-p":
			mode = "print"
			if i+1 < len(args) {
				printPrompt = args[i+1]
				i++
			}
		default:
			extra = append(extra, args[i])
		}
	}
	// Post-pass: resolve --model now that --core is known.
	// When --core codex/grok, the model is a backend-side name that may not
	// live in any provider model list, so we skip fuzzy resolution and pass
	// it through unchanged. The server hands it to the backend's resolver.
	if rawModel != "" {
		if overrideCore == "codex" || overrideCore == "grok" || overrideCore == "kimi" || overrideCore == "agy" {
			overrideModel = rawModel
		} else {
			resolved, err := resolveFuzzyModel(rawModel)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[%s] %v\n", appName, err)
				os.Exit(1)
			}
			if resolved != "" {
				overrideModel = resolved
			}
		}
	}
	return
}
