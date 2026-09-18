package app

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ── Hook translator: event-aware system-reminder injection ──
//
// Claude Code invokes this binary with a JSON payload on stdin for any hook
// event (PreToolUse, UserPromptSubmit, SessionStart, Stop, ...). We dispatch
// on hook_event_name and run the matching rules from YAML.
//
//   PreToolUse         → tool_name + file path glob (ToolHookRule.Tools / .Match)
//   PostToolUse        → tool_name + response/input regex
//   UserPromptSubmit   → prompt regex (ToolHookRule.MatchPrompt)
//   Stop / SubagentStop → events list only — fire whenever rule lists event
//   (future events)    → added case by case
//
// Any stdout the hook produces is injected as system-reminder context.
// Exit 0 = allow, non-zero = block (we never block; injection is advisory).
//
// Every invocation writes one row to tool_hook_audit. The table doubles as
// dedup state (per_session / per_file): dedup checks query WHERE injected=1.

// Supported hook event names — mirrors Claude Code's settings.json-level
// allowlist in src/utils/settings/settings.ts (as of upstream 2026-04).
// All are audited; only the ones with bespoke matchers (PreToolUse,
// PostToolUse, UserPromptSubmit, Stop, SubagentStop) run rule matching.
// Everything else is audit-only.
const (
	HookEventPreToolUse       = "PreToolUse"
	HookEventPostToolUse      = "PostToolUse"
	HookEventNotification     = "Notification"
	HookEventUserPromptSubmit = "UserPromptSubmit"
	HookEventSessionStart     = "SessionStart"
	HookEventSessionEnd       = "SessionEnd"
	HookEventStop             = "Stop"
	HookEventSubagentStop     = "SubagentStop"
	HookEventPreCompact       = "PreCompact"
	HookEventPostCompact      = "PostCompact"
	HookEventTeammateIdle     = "TeammateIdle"
	HookEventTaskCreated      = "TaskCreated"
	HookEventTaskCompleted    = "TaskCompleted"

	// Server-side only (not a CC hook event — evaluated by weiran server,
	// consumed by tool-hook binary via content_hook_pending table).
	// Listed here so `weiran tool-hook events` and rule validation recognize it.
	HookEventPostAssistantMsg = "PostAssistantMsg"
)

// AllHookEvents is the ordered list Claude Code accepts at the top level of
// settings.json's `hooks` object. Kept in sync with Claude Code upstream.
// Adding a new event here also requires adding it to settings.json for the
// binary to be invoked; otherwise no audit is written for it.
var AllHookEvents = []string{
	HookEventPreToolUse,
	HookEventPostToolUse,
	HookEventNotification,
	HookEventUserPromptSubmit,
	HookEventSessionStart,
	HookEventSessionEnd,
	HookEventStop,
	HookEventSubagentStop,
	HookEventPreCompact,
	HookEventPostCompact,
	HookEventTeammateIdle,
	HookEventTaskCreated,
	HookEventTaskCompleted,
}

// knownHookEvents is the set form of AllHookEvents for O(1) lookup.
// Events outside this set are still audited but flagged as "unknown event"
// so we notice when Claude Code adds a new hook we haven't wired.
// Also includes server-side-only events (PostAssistantMsg) for rule validation.
var knownHookEvents = func() map[string]bool {
	m := make(map[string]bool, len(AllHookEvents)+1)
	for _, e := range AllHookEvents {
		m[e] = true
	}
	// Server-side events not in CC's settings.json but valid in tool-hooks.yaml
	m[HookEventPostAssistantMsg] = true
	return m
}()

// defaultEvent is assumed when a rule's Events list is empty — preserves
// backward compatibility with the original path-only tool-hook YAML.
const defaultEvent = HookEventPreToolUse

// ToolHookInput mirrors the payload Claude Code sends via stdin. Fields are a
// union across supported events; unused ones are zero.
type ToolHookInput struct {
	SessionID      string          `json:"session_id"`
	CWD            string          `json:"cwd"`
	ToolName       string          `json:"tool_name"`       // PreToolUse / PostToolUse
	ToolInput      json.RawMessage `json:"tool_input"`      // PreToolUse / PostToolUse
	ToolResponse   json.RawMessage `json:"tool_response"`   // PostToolUse
	Prompt         string          `json:"prompt"`          // UserPromptSubmit
	HookEventName  string          `json:"hook_event_name"`
	TranscriptPath string          `json:"transcript_path"`
}

// ToolHookRule is one YAML rule definition.
type ToolHookRule struct {
	ID            string   `yaml:"id"`
	Events        []string `yaml:"events"`         // which hook events the rule applies to; empty = [PreToolUse]
	Match         []string `yaml:"match"`          // glob patterns against target path (PreToolUse)
	Tools         []string `yaml:"tools"`          // Read / Edit / Write / Grep / Glob; empty = any (PreToolUse / PostToolUse)
	MatchPrompt   []string `yaml:"match_prompt"`   // regex patterns against user prompt (UserPromptSubmit)
	MatchResponse []string `yaml:"match_response"` // regex patterns against tool_response JSON (PostToolUse)
	MatchInput    []string `yaml:"match_input"`    // regex patterns against tool_input JSON (PreToolUse) — used when tool has no path (e.g. Bash)
	SkipInput     []string `yaml:"skip_input"`     // regex patterns against tool_input JSON (Pre + PostToolUse) — match ⇒ skip rule
	Inject        string   `yaml:"inject"`         // system-reminder body (trimmed) — also used as permissionDecisionReason when Decision != ""
	Decision      string   `yaml:"decision"`       // PreToolUse only: "deny" | "ask" | "allow". Empty (default) = additionalContext (legacy behavior).
	Action        string   `yaml:"action"`         // PreToolUse only: side-effect to run when rule matches. Currently supports "mark_restart_initiator". Empty = no side effect (default). Runs in addition to Inject (both fire).
	Dedupe        string   `yaml:"dedupe"`         // never | per_session | per_file (default per_file)
	Budget        int      `yaml:"budget"`         // per-rule max chars; 0 = 500 default
	Priority      int      `yaml:"priority"`       // higher fires first; 0 = 50 default
	Disabled      bool     `yaml:"disabled"`
	// DisableUntil / DisableReason implement the time-bounded temporary disable.
	// The model can call `weiran tool-hook disable <id> --until +30m --reason …`
	// to grant itself a short exemption when a rule is judged inapplicable to
	// the current task. When DisableUntil is in the future, the rule is skipped
	// and an audit row is written with SkipReason="temp_disabled". When it has
	// expired, the rule self-heals (fires normally) — no cleanup required.
	// Hard cap of 2h is enforced at disable time, not at eval time.
	DisableUntil  *time.Time `yaml:"disable_until,omitempty"`
	DisableReason string     `yaml:"disable_reason,omitempty"`
}

// validDecision returns true if d is one of the Claude Code permissionDecision
// values we accept on a rule. Empty = no decision (context injection only).
func validDecision(d string) bool {
	switch d {
	case "", "deny", "ask", "allow":
		return true
	}
	return false
}

// ToolHookConfig is the top-level YAML doc.
type ToolHookConfig struct {
	Budget int            `yaml:"budget"` // global max chars per hook call; 0 = 1500 default
	Rules  []ToolHookRule `yaml:"rules"`
}

// defaultToolHookConfigPath returns the rules YAML location.
// Override with <APPNAME>_TOOL_HOOKS env var.
func defaultToolHookConfigPath() string {
	if v := os.Getenv(strings.ToUpper(appName) + "_TOOL_HOOKS"); v != "" {
		return v
	}
	return filepath.Join(workspace, "tool-hooks.yaml")
}

// extractToolPath pulls the identifying string from tool_input based on tool name.
// For file tools this is file_path / path / notebook_path.
// For the Skill tool we return the skill name so tool_hook_audit.path doubles as
// per-skill invocation telemetry (`SELECT path, COUNT(*) ... WHERE tool_name='Skill'`).
// Returns "" if the tool has no identifier we care about.
func extractToolPath(tool string, input json.RawMessage) string {
	var probe struct {
		FilePath     string `json:"file_path"`
		Path         string `json:"path"`
		NotebookPath string `json:"notebook_path"`
		Skill        string `json:"skill"`
	}
	_ = json.Unmarshal(input, &probe)
	if tool == "Skill" && probe.Skill != "" {
		return probe.Skill
	}
	switch {
	case probe.FilePath != "":
		return probe.FilePath
	case probe.NotebookPath != "":
		return probe.NotebookPath
	case probe.Path != "":
		return probe.Path
	}
	return ""
}

// matchesGlob checks if path matches any of the glob patterns. Supports:
//   - leading ~ expansion
//   - ** for deep match (via prefix/suffix split)
//   - filepath.Match semantics for single segment
func matchesGlob(path string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	normalized := path
	if home := os.Getenv("HOME"); home != "" && strings.HasPrefix(normalized, home) {
		// keep as-is; patterns may use either ~ or absolute
	}
	for _, pat := range patterns {
		if home := os.Getenv("HOME"); home != "" && strings.HasPrefix(pat, "~/") {
			pat = filepath.Join(home, pat[2:])
		}
		if globMatch(pat, normalized) {
			return true
		}
	}
	return false
}

// globMatch translates a glob pattern to a regex and matches against s.
//   ** → matches any sequence of characters including /
//   *  → matches any sequence except /
//   ?  → matches a single non-/ char
// Everything else is quoted literally. Pattern must match the full string.
//
// Special case: when the pattern starts with "**/", zero path segments
// (i.e. plain basename match) are also accepted — so "**/*.jsonl" matches
// both "a/b/foo.jsonl" and "foo.jsonl".
func globMatch(pattern, s string) bool {
	rx := compileGlobRegex(pattern)
	if rx == nil {
		return false
	}
	if rx.MatchString(s) {
		return true
	}
	// Convenience: if the pattern has no slash at all (e.g. "*.go"),
	// also try it against the basename.
	if !strings.Contains(pattern, "/") {
		return rx.MatchString(filepath.Base(s))
	}
	return false
}

var globCache sync.Map // pattern → *regexp.Regexp

func compileGlobRegex(pattern string) *regexp.Regexp {
	if v, ok := globCache.Load(pattern); ok {
		return v.(*regexp.Regexp)
	}
	rx := buildGlobRegex(pattern)
	globCache.Store(pattern, rx)
	return rx
}

func buildGlobRegex(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	// Special prefix: "**/" allows zero segments. We express this in the
	// regex as "(?:.*/)?" instead of ".*/" so it can match the bare basename.
	rest := pattern
	if strings.HasPrefix(rest, "**/") {
		b.WriteString("(?:.*/)?")
		rest = rest[3:]
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch c {
		case '*':
			if i+1 < len(rest) && rest[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	rx, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return rx
}

// loadToolHookConfig reads and parses the YAML. Missing file → empty config.
func loadToolHookConfig(path string) (*ToolHookConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ToolHookConfig{}, nil
		}
		return nil, err
	}
	var cfg ToolHookConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Budget == 0 {
		cfg.Budget = 1500
	}
	for i := range cfg.Rules {
		if cfg.Rules[i].Budget == 0 {
			cfg.Rules[i].Budget = 500
		}
		if cfg.Rules[i].Priority == 0 {
			cfg.Rules[i].Priority = 50
		}
		if cfg.Rules[i].Dedupe == "" {
			cfg.Rules[i].Dedupe = "per_file"
		}
	}
	sort.SliceStable(cfg.Rules, func(i, j int) bool {
		return cfg.Rules[i].Priority > cfg.Rules[j].Priority
	})
	return &cfg, nil
}

// toolMatches returns true if the rule's Tools whitelist includes this tool.
// Empty Tools = match any.
func (r *ToolHookRule) toolMatches(tool string) bool {
	if len(r.Tools) == 0 {
		return true
	}
	for _, t := range r.Tools {
		if strings.EqualFold(t, tool) {
			return true
		}
	}
	return false
}

// eventMatches returns true if the rule applies to the given hook event.
// Empty Events list defaults to [PreToolUse] — keeps legacy rules working
// without a YAML migration.
func (r *ToolHookRule) eventMatches(event string) bool {
	if len(r.Events) == 0 {
		return event == defaultEvent
	}
	for _, e := range r.Events {
		if strings.EqualFold(e, event) {
			return true
		}
	}
	return false
}

// matchesPromptRegex returns true if prompt matches any of the regex patterns.
// Patterns are compiled on first use and cached. Invalid patterns are skipped.
func matchesPromptRegex(prompt string, patterns []string) bool {
	if prompt == "" || len(patterns) == 0 {
		return false
	}
	for _, pat := range patterns {
		rx := compilePromptRegex(pat)
		if rx != nil && rx.MatchString(prompt) {
			return true
		}
	}
	return false
}

// matchesInputRegex returns true if the tool_input matches any of the regex
// patterns. Matching is done twice — against the raw JSON bytes AND against
// string-valued fields ("command", "new_string", "content", "pattern", "query")
// after JSON decoding. This dual match is necessary because JSON encoders may
// escape `<`, `>`, `&` as `<` / `>` / `&` (Go's encoding/json
// default; not Node's) — a regex like `>` written naively against raw bytes
// misses every `>` character that got escaped. Decoded-field matching restores
// the original character so rule authors can write intuitive regexes like `>`
// and have them work regardless of the encoder.
//
// Used by:
//   - Pre + PostToolUse rules with `match_input:` (fire when tool_input matches)
//   - Pre + PostToolUse rules with `skip_input:` (suppress rule when tool_input matches,
//     e.g. daily-notes Edit whose new_string is a heartbeat header, or
//     `echo '... weiran notify ...'` / `weiran tool-hook` smoke tests on Bash).
// Reuses compilePromptRegex for pattern caching.
func matchesInputRegex(input []byte, patterns []string) bool {
	if len(input) == 0 || len(patterns) == 0 {
		return false
	}
	raw := string(input)
	decoded := extractStringFields(input)
	for _, pat := range patterns {
		rx := compilePromptRegex(pat)
		if rx == nil {
			continue
		}
		if rx.MatchString(raw) {
			return true
		}
		for _, s := range decoded {
			if rx.MatchString(s) {
				return true
			}
		}
	}
	return false
}

// extractStringFields pulls out the string values we care about for regex
// matching. We intentionally don't walk arbitrary JSON — the cost isn't worth
// it for an interactive hook that runs on every Bash/Edit call — but we do
// cover the fields that actually appear in Claude Code tool_input payloads:
//
//   Bash       → command
//   Edit/Write → old_string, new_string, content
//   Grep       → pattern
//   Glob       → pattern
//
// When a tool has none of these (e.g. Read), decoded is empty and we fall back
// to raw-bytes matching only.
func extractStringFields(input []byte) []string {
	var probe struct {
		Command   string `json:"command"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Content   string `json:"content"`
		Pattern   string `json:"pattern"`
		Query     string `json:"query"`
	}
	if err := json.Unmarshal(input, &probe); err != nil {
		return nil
	}
	out := make([]string, 0, 6)
	for _, s := range []string{probe.Command, probe.OldString, probe.NewString, probe.Content, probe.Pattern, probe.Query} {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// matchesResponseRegex returns true if the tool_response (as raw JSON bytes)
// matches any of the regex patterns. Reuses compilePromptRegex for pattern
// caching — regexps are pattern strings, independent of target semantics.
func matchesResponseRegex(response []byte, patterns []string) bool {
	if len(response) == 0 || len(patterns) == 0 {
		return false
	}
	s := string(response)
	for _, pat := range patterns {
		rx := compilePromptRegex(pat)
		if rx != nil && rx.MatchString(s) {
			return true
		}
	}
	return false
}

var promptRegexCache sync.Map // pattern → *regexp.Regexp (or nil sentinel on bad pattern)

type badRegex struct{}

var badRegexSentinel = &badRegex{}

func compilePromptRegex(pattern string) *regexp.Regexp {
	if v, ok := promptRegexCache.Load(pattern); ok {
		if rx, ok := v.(*regexp.Regexp); ok {
			return rx
		}
		return nil // sentinel = known-bad
	}
	rx, err := regexp.Compile(pattern)
	if err != nil {
		promptRegexCache.Store(pattern, badRegexSentinel)
		return nil
	}
	promptRegexCache.Store(pattern, rx)
	return rx
}

// isDeduped checks the audit table for a prior successful injection of this
// rule, scoped by dedupe policy.
func isDeduped(db *sql.DB, rule ToolHookRule, sessionID, path string) bool {
	switch rule.Dedupe {
	case "never":
		return false
	case "per_session":
		var n int
		db.QueryRow(`SELECT 1 FROM tool_hook_audit WHERE rule_id=? AND session_id=? AND injected=1 LIMIT 1`,
			rule.ID, sessionID).Scan(&n)
		return n == 1
	case "per_file":
		fallthrough
	default:
		var n int
		// per_file dedupe is session-scoped so a new session can re-injection
		// the same file — intentional: new session = new context.
		db.QueryRow(`SELECT 1 FROM tool_hook_audit WHERE rule_id=? AND session_id=? AND path=? AND injected=1 LIMIT 1`,
			rule.ID, sessionID, path).Scan(&n)
		return n == 1
	}
}

// writeToolHookAudit persists one row.
func writeToolHookAudit(db *sql.DB, row toolHookAuditRow) {
	if db == nil {
		return
	}
	if row.EventName == "" {
		row.EventName = defaultEvent
	}
	_, err := db.Exec(`INSERT INTO tool_hook_audit
		(timestamp, session_id, cwd, event_name, tool_name, path, rule_id, injected, skip_reason, injection_size, budget_used, latency_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.Timestamp, row.SessionID, row.CWD, row.EventName, row.ToolName, row.Path,
		row.RuleID, row.Injected, row.SkipReason, row.InjectionSize, row.BudgetUsed, row.LatencyMS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[tool-hook] audit write failed: %v\n", err)
	}
}

type toolHookAuditRow struct {
	Timestamp     string
	SessionID     string
	CWD           string
	EventName     string
	ToolName      string
	Path          string
	RuleID        string
	Injected      bool
	SkipReason    string
	InjectionSize int
	BudgetUsed    int
	LatencyMS     int64
}

// runToolHook is the entry point when weiran is invoked as a Claude Code hook.
// Reads JSON from stdin, dispatches on hook_event_name, writes optional
// system-reminder JSON to stdout. Never errors — hook failures must not block.
func runToolHook() {
	if os.Getenv("WEIRAN_HOOK_TRACE") == "1" {
		traceStart := time.Now()
		defer func() {
			fmt.Fprintf(os.Stderr, "[trace] total=%s\n", time.Since(traceStart))
		}()
	}

	rawIn, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	if os.Getenv("WEIRAN_HOOK_TRACE") == "1" {
		fmt.Fprintf(os.Stderr, "[trace] stdin_read ok\n")
	}
	var in ToolHookInput
	if err := json.Unmarshal(rawIn, &in); err != nil {
		return
	}

	// Default legacy payloads (no hook_event_name) to PreToolUse so older
	// Claude Code versions keep working unchanged.
	event := in.HookEventName
	if event == "" {
		event = defaultEvent
	}

	cfgPath := defaultToolHookConfigPath()
	cfg, err := loadToolHookConfig(cfgPath)
	if err != nil {
		// Config-level failure — affects every subsequent hook invocation.
		// Surface to the active session UI so we don't silently break tool
		// gating after a yaml typo.
		hookErrorf("__config__", "error", in, "config load error: %v", err)
		return
	}

	db, _ := openDB()
	if db != nil {
		defer db.Close()
	}

	// Auto-cleanup expired temp disables so yaml doesn't accumulate dead state.
	// Fast path (no expired entries) is zero IO — only touches disk when there's
	// something to clean. Multiple concurrent hook invocations may both try to
	// clean; last-writer-wins is safe (all cleanups are idempotent).
	cleanupExpiredDisables(cfgPath, cfg, db)

	switch event {
	case HookEventPreToolUse:
		runPreToolUseHook(in, cfg, db)
	case HookEventUserPromptSubmit:
		runUserPromptSubmitHook(in, cfg, db)
	case HookEventPostToolUse:
		runPostToolUseHook(in, cfg, db)
	case HookEventStop, HookEventSubagentStop:
		runStopHook(event, in, cfg, db)
	default:
		// All other events (SessionStart, Notification, PreCompact, ...) are
		// audit-only for now: one row per invocation, no injection.
		// Future bespoke matchers can be added by name.
		runAuditOnlyHook(event, in, db)
	}
}

// runAuditOnlyHook records a single observability row for events that don't
// (yet) have a rule matcher. The audit row still captures session_id / cwd /
// event_name / tool_name (if PostToolUse) so we can see what Claude Code does.
func runAuditOnlyHook(event string, in ToolHookInput, db *sql.DB) {
	if db == nil {
		return
	}
	start := time.Now()
	// For PostToolUse we keep the tool_name + path so tool usage is observable
	// end-to-end; for other events those stay empty.
	var toolName, path string
	if event == HookEventPostToolUse {
		toolName = in.ToolName
		path = extractToolPath(in.ToolName, in.ToolInput)
	}
	writeToolHookAudit(db, toolHookAuditRow{
		Timestamp: time.Now().Format(time.RFC3339),
		SessionID: in.SessionID,
		CWD:       in.CWD,
		EventName: event,
		ToolName:  toolName,
		Path:      path,
		LatencyMS: time.Since(start).Milliseconds(),
	})
}

// runPreToolUseHook handles the original path + tool glob matching.
func runPreToolUseHook(in ToolHookInput, cfg *ToolHookConfig, db *sql.DB) {
	start := time.Now()
	path := extractToolPath(in.ToolName, in.ToolInput)
	sessionID := in.SessionID

	audited := false
	budgetRemaining := cfg.Budget
	var injections []string
	budgetUsedTotal := 0

	// Drain any pending PostAssistantMsg content hook injections (written by
	// the server when it detected keywords in mid-turn assistant text).
	if pending := drainContentHookPending(db, sessionID); len(pending) > 0 {
		injections = append(injections, pending...)
		for _, p := range pending {
			budgetUsedTotal += len(p)
			budgetRemaining -= len(p)
		}
	}

	for _, rule := range cfg.Rules {
		if rule.Disabled || rule.ID == "" {
			continue
		}
		if isTempDisabled(&rule) {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				EventName:  HookEventPreToolUse,
				ToolName:   in.ToolName,
				Path:       path,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: tempDisableReason(&rule),
				LatencyMS:  time.Since(start).Milliseconds(),
			})
			continue
		}
		if !rule.eventMatches(HookEventPreToolUse) {
			continue
		}
		if !rule.toolMatches(in.ToolName) {
			continue
		}
		// Match semantics: if `match` is set, path must match; if `match_input`
		// is set, tool_input JSON must match. Both empty ⇒ rule inert (prevents
		// accidentally global rules). When both set ⇒ both must match (AND).
		hasPathMatch := len(rule.Match) > 0
		hasInputMatch := len(rule.MatchInput) > 0
		if !hasPathMatch && !hasInputMatch {
			continue
		}
		if hasPathMatch && !matchesGlob(path, rule.Match) {
			continue
		}
		if hasInputMatch && !matchesInputRegex(in.ToolInput, rule.MatchInput) {
			continue
		}
		audited = true

		if matchesInputRegex(in.ToolInput, rule.SkipInput) {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				EventName:  HookEventPreToolUse,
				ToolName:   in.ToolName,
				Path:       path,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: "skip_input",
				LatencyMS:  time.Since(start).Milliseconds(),
			})
			continue
		}

		if db != nil && isDeduped(db, rule, sessionID, path) {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				EventName:  HookEventPreToolUse,
				ToolName:   in.ToolName,
				Path:       path,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: "dedupe",
				LatencyMS:  time.Since(start).Milliseconds(),
			})
			continue
		}

		body := strings.TrimSpace(rule.Inject)
		if len(body) > rule.Budget {
			body = body[:rule.Budget]
		}

		// Decision rules (deny/ask/allow) short-circuit immediately — permission
		// decisions are terminal, so there's no point accumulating further
		// additionalContext after one fires. Rules are already sorted by priority
		// DESC, so the highest-priority decision wins. Budget check is still
		// enforced: an oversized decision reason falls back to skip (budget) and
		// moves on — a partial deny reason would confuse the user.
		if rule.Decision != "" {
			if !validDecision(rule.Decision) {
				hookErrorf(rule.ID, "error", in, "ignoring invalid decision %q (allowed: deny, ask, allow)", rule.Decision)
				continue
			}
			if len(body) > budgetRemaining {
				writeToolHookAudit(db, toolHookAuditRow{
					Timestamp:  time.Now().Format(time.RFC3339),
					SessionID:  sessionID,
					CWD:        in.CWD,
					EventName:  HookEventPreToolUse,
					ToolName:   in.ToolName,
					Path:       path,
					RuleID:     rule.ID,
					Injected:   false,
					SkipReason: "budget",
					LatencyMS:  time.Since(start).Milliseconds(),
				})
				continue
			}
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:     time.Now().Format(time.RFC3339),
				SessionID:     sessionID,
				CWD:           in.CWD,
				EventName:     HookEventPreToolUse,
				ToolName:      in.ToolName,
				Path:          path,
				RuleID:        rule.ID,
				Injected:      true,
				InjectionSize: len(body),
				BudgetUsed:    len(body),
				LatencyMS:     time.Since(start).Milliseconds(),
			})
			emitDecision(HookEventPreToolUse, rule.Decision, fmt.Sprintf("[rule:%s] %s", rule.ID, body))
			return
		}

		if len(body) > budgetRemaining {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				EventName:  HookEventPreToolUse,
				ToolName:   in.ToolName,
				Path:       path,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: "budget",
				LatencyMS:  time.Since(start).Milliseconds(),
			})
			continue
		}

		injections = append(injections, fmt.Sprintf("[rule:%s] %s", rule.ID, body))
		budgetUsedTotal += len(body)
		budgetRemaining -= len(body)

		writeToolHookAudit(db, toolHookAuditRow{
			Timestamp:     time.Now().Format(time.RFC3339),
			SessionID:     sessionID,
			CWD:           in.CWD,
			EventName:     HookEventPreToolUse,
			ToolName:      in.ToolName,
			Path:          path,
			RuleID:        rule.ID,
			Injected:      true,
			InjectionSize: len(body),
			BudgetUsed:    budgetUsedTotal,
			LatencyMS:     time.Since(start).Milliseconds(),
		})

		// Side-effect dispatch — runs after audit so the action's own logging
		// can correlate. Action failures are non-fatal: we log to stderr but
		// don't abort the hook (the user's tool call must proceed).
		if rule.Action != "" {
			runRuleAction(rule.Action, rule.ID, in)
		}
	}

	if !audited && db != nil {
		writeToolHookAudit(db, toolHookAuditRow{
			Timestamp: time.Now().Format(time.RFC3339),
			SessionID: sessionID,
			CWD:       in.CWD,
			EventName: HookEventPreToolUse,
			ToolName:  in.ToolName,
			Path:      path,
			LatencyMS: time.Since(start).Milliseconds(),
		})
	}

	emitInjections(HookEventPreToolUse, injections)
}

// runRuleAction dispatches a rule's `action:` side effect. Unknown actions are
// logged to stderr and ignored (forward-compat: yaml may reference an action
// that this binary doesn't know yet).
//
// All actions are best-effort: failures log to stderr but never propagate —
// the user's underlying tool call must always proceed. The hook is advisory.
func runRuleAction(action, ruleID string, in ToolHookInput) {
	switch action {
	case "mark_restart_initiator":
		actionMarkRestartInitiator(ruleID, in)
	case "record_fragment_load":
		actionRecordFragmentLoad(ruleID, in)
	default:
		hookErrorf(ruleID, "error", in, "unknown action %q (allowed: mark_restart_initiator, record_fragment_load)", action)
	}
}

// actionRecordFragmentLoad notifies the running weiran server that the agent
// has just Read a soul fragment file via the Read tool. The server appends
// the path to the session's LoadedFragments set so the topic-based fallback
// detector (server_soul_patch.go: prepareSoulPatch) doesn't double-patch a
// fragment the agent already has in conversation history.
//
// Best-effort: server unreachable, missing env vars, non-2xx responses all
// log to stderr but never block the underlying Read tool — the user-facing
// flow must always proceed.
//
// Path resolution: extractToolPath returns the file_path argument that Read
// was invoked with. We require it to live under the soul fragments dir
// (rule.match already constrains this, but we double-check defense-in-depth)
// and resolve it to an absolute path matching what listAllFragmentMetas
// returns, so server-side `LoadedFragments` set membership comparisons hit.
func actionRecordFragmentLoad(ruleID string, in ToolHookInput) {
	path := extractToolPath(in.ToolName, in.ToolInput)
	if path == "" {
		fmt.Fprintf(os.Stderr, "[tool-hook] rule %s: no path in tool_input, skipping\n", ruleID)
		return
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}

	if in.SessionID == "" {
		fmt.Fprintf(os.Stderr, "[tool-hook] rule %s: no session_id, skipping fragment record\n", ruleID)
		return
	}
	weiranSID := getWeiranSessionIDByClaudeSID(in.SessionID)
	if weiranSID == "" {
		// Fall back: maybe the env-injected WEIRAN_SESSION_ID matches directly.
		weiranSID = os.Getenv("WEIRAN_SESSION_ID")
	}
	if weiranSID == "" {
		fmt.Fprintf(os.Stderr, "[tool-hook] rule %s: no weiran session for cc=%s, skipping fragment record\n",
			ruleID, shortID(in.SessionID))
		return
	}

	serverURL := os.Getenv("WEIRAN_SERVER_URL")
	authToken := os.Getenv("WEIRAN_AUTH_TOKEN")
	if serverURL == "" {
		// Fallback to local default — the daemon binds 127.0.0.1:6789 by default.
		serverURL = "http://127.0.0.1:6789"
	}

	bodyJSON, _ := json.Marshal(map[string]string{"path": path})
	req, err := http.NewRequest("POST",
		strings.TrimRight(serverURL, "/")+"/api/sessions/"+weiranSID+"/loaded-fragment",
		bytes.NewReader(bodyJSON))
	if err != nil {
		hookErrorf(ruleID, "error", in, "build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		hookErrorf(ruleID, "error", in, "post fragment-load failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		hookErrorf(ruleID, "error", in, "server returned %d for fragment-load", resp.StatusCode)
		return
	}
	fmt.Fprintf(os.Stderr, "[tool-hook] rule %s: recorded fragment load weiran=%s path=%s\n",
		ruleID, shortID(weiranSID), filepath.Base(path))
}

// reportHookErrorToServer ships a hook failure to the running soul-cli
// server so the active web UI can toast it. This is best-effort and
// async-tolerant: stderr stays the durable log; the SSE event is just a
// fast user-facing surface. Successful hook runs do NOT call this — the
// UI stays quiet for the happy path.
//
// Routing: cc session_id → app session_id → POST hook-error.
// If we can't resolve an app session, we fall back to the env-injected
// <APPNAME>_SESSION_ID (set by `<binary> server` when spawning Claude
// Code), so even hooks fired before the cc handshake DB row is written
// can still reach the right session.
func reportHookErrorToServer(ruleID, level, msg string, in ToolHookInput) {
	prefix := ipcEnvPrefix()
	appSID := ""
	if in.SessionID != "" {
		appSID = getWeiranSessionIDByClaudeSID(in.SessionID)
	}
	if appSID == "" {
		appSID = os.Getenv(prefix + "_SESSION_ID")
	}
	if appSID == "" {
		// No session to route to — stderr already has the line.
		return
	}
	serverURL := os.Getenv(prefix + "_SERVER_URL")
	if serverURL == "" {
		// No URL — caller hasn't injected it; stderr is the only sink.
		return
	}
	authToken := os.Getenv(prefix + "_AUTH_TOKEN")

	bodyJSON, _ := json.Marshal(map[string]string{
		"rule_id":   ruleID,
		"event":     in.HookEventName,
		"tool_name": in.ToolName,
		"level":     level,
		"msg":       msg,
	})
	req, err := http.NewRequest("POST",
		strings.TrimRight(serverURL, "/")+"/api/sessions/"+appSID+"/hook-error",
		bytes.NewReader(bodyJSON))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	// 1s — hook execution must not be slowed by reporting. If the server is
	// down or busy, fall back silently to stderr.
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// hookErrorf is a stderr+UI dual writer: stderr for durable logs, server
// for live UI toasts. Format matches fmt.Fprintf conventions.
func hookErrorf(ruleID, level string, in ToolHookInput, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "[tool-hook] rule %s: %s\n", ruleID, msg)
	reportHookErrorToServer(ruleID, level, msg, in)
}

// actionMarkRestartInitiator persists a custom rehydrate_message for the
// session that ran a server-restart command. After the server comes back, the
// rehydration path uses this custom message instead of the bystander warning,
// so the model recognizes the restart as something IT triggered (not an
// external interruption).
//
// Why this exists: PreToolUse fires before Bash executes. By the time
// `make server-restart` kills the server process, the DB write has already
// landed. On rehydration, server_session.go:1917 reads the rehydrate_message
// column and sends it to the model as the wake message.
//
// Lookup chain: CC session_id (from hook payload) → weiran session_id (via
// server_sessions.claude_session_id) → setRehydrateMessage. If lookup fails
// (session not yet handshaked, hook fired outside any session, etc.), we log
// and return — falling back to the bystander warning is safe.
func actionMarkRestartInitiator(ruleID string, in ToolHookInput) {
	if in.SessionID == "" {
		fmt.Fprintf(os.Stderr, "[tool-hook] rule %s: no session_id in payload, skipping\n", ruleID)
		return
	}
	weiranSID := getWeiranSessionIDByClaudeSID(in.SessionID)
	if weiranSID == "" {
		fmt.Fprintf(os.Stderr, "[tool-hook] rule %s: no weiran session for cc=%s, skipping\n",
			ruleID, shortID(in.SessionID))
		return
	}
	cmd := extractBashCommand(in.ToolInput)
	if cmd == "" {
		cmd = "(server restart command)"
	}
	if len(cmd) > 200 {
		cmd = cmd[:200] + "…"
	}
	msg := fmt.Sprintf(
		"Server restart you triggered via `%s` completed. "+
			"Continue your work — do NOT re-run the restart, it already happened. "+
			"Any in-flight tool calls from before the restart did not complete; "+
			"verify state if needed before assuming success.",
		cmd,
	)
	setRehydrateMessage(weiranSID, msg)
	fmt.Fprintf(os.Stderr, "[tool-hook] rule %s: marked weiran=%s as restart initiator (cmd=%q)\n",
		ruleID, shortID(weiranSID), cmd)
}

// extractBashCommand pulls the `command` field out of a Bash tool_input JSON
// blob. Returns "" if absent or unparseable. Used for human-readable context
// in side-effect actions (e.g. mark_restart_initiator) — never for matching
// (use matchesInputRegex for that).
func extractBashCommand(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var probe struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &probe); err != nil {
		return ""
	}
	return strings.TrimSpace(probe.Command)
}

// runUserPromptSubmitHook handles prompt regex matching for the user's message.
// Dedupe scope uses session_id only (no "path" concept for UserPromptSubmit);
// the audit table's path column is reused as a short prompt digest (first 120
// chars) for observability.
func runUserPromptSubmitHook(in ToolHookInput, cfg *ToolHookConfig, db *sql.DB) {
	start := time.Now()
	sessionID := in.SessionID
	digest := promptDigest(in.Prompt, 120)

	audited := false
	budgetRemaining := cfg.Budget
	var injections []string
	budgetUsedTotal := 0

	for _, rule := range cfg.Rules {
		if rule.Disabled || rule.ID == "" {
			continue
		}
		if isTempDisabled(&rule) {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				EventName:  HookEventUserPromptSubmit,
				Path:       digest,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: tempDisableReason(&rule),
				LatencyMS:  time.Since(start).Milliseconds(),
			})
			continue
		}
		if !rule.eventMatches(HookEventUserPromptSubmit) {
			continue
		}
		if !matchesPromptRegex(in.Prompt, rule.MatchPrompt) {
			continue
		}
		audited = true

		// For UserPromptSubmit we dedupe per_session (and per_file is treated
		// as per_session too since there is no file path).
		if db != nil && isDedupedSessionScoped(db, rule, sessionID, HookEventUserPromptSubmit) {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				EventName:  HookEventUserPromptSubmit,
				Path:       digest,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: "dedupe",
				LatencyMS:  time.Since(start).Milliseconds(),
			})
			continue
		}

		body := strings.TrimSpace(rule.Inject)
		if len(body) > rule.Budget {
			body = body[:rule.Budget]
		}
		if len(body) > budgetRemaining {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				EventName:  HookEventUserPromptSubmit,
				Path:       digest,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: "budget",
				LatencyMS:  time.Since(start).Milliseconds(),
			})
			continue
		}

		injections = append(injections, fmt.Sprintf("[rule:%s] %s", rule.ID, body))
		budgetUsedTotal += len(body)
		budgetRemaining -= len(body)

		writeToolHookAudit(db, toolHookAuditRow{
			Timestamp:     time.Now().Format(time.RFC3339),
			SessionID:     sessionID,
			CWD:           in.CWD,
			EventName:     HookEventUserPromptSubmit,
			Path:          digest,
			RuleID:        rule.ID,
			Injected:      true,
			InjectionSize: len(body),
			BudgetUsed:    budgetUsedTotal,
			LatencyMS:     time.Since(start).Milliseconds(),
		})
	}

	if !audited && db != nil {
		writeToolHookAudit(db, toolHookAuditRow{
			Timestamp: time.Now().Format(time.RFC3339),
			SessionID: sessionID,
			CWD:       in.CWD,
			EventName: HookEventUserPromptSubmit,
			Path:      digest,
			LatencyMS: time.Since(start).Milliseconds(),
		})
	}

	emitInjections(HookEventUserPromptSubmit, injections)
}

// runStopHook handles Stop and SubagentStop events. Neither carries a tool
// name, file path, or prompt to match against, so a rule fires whenever it
// lists the event in `events:` and passes dedupe/budget checks. Use this for
// end-of-turn behavioral reminders that should reach the next turn's context
// (Claude Code surfaces additionalContext from Stop hooks at the start of
// the next user turn).
//
// Dedupe semantics: per_session = one inject per session (use for one-shot
// orientation that doesn't need re-stating); never = inject every stop (use
// for behavior anchors that should re-prime each turn). per_file is treated
// as per_session since Stop events have no path surrogate.
func runStopHook(event string, in ToolHookInput, cfg *ToolHookConfig, db *sql.DB) {
	start := time.Now()
	sessionID := in.SessionID
	digest := "" // Stop events have no meaningful path

	audited := false
	budgetRemaining := cfg.Budget
	var injections []string
	budgetUsedTotal := 0

	for _, rule := range cfg.Rules {
		if rule.Disabled || rule.ID == "" {
			continue
		}
		if isTempDisabled(&rule) {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				EventName:  event,
				Path:       digest,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: tempDisableReason(&rule),
				LatencyMS:  time.Since(start).Milliseconds(),
			})
			continue
		}
		if !rule.eventMatches(event) {
			continue
		}
		audited = true

		// Dedupe: session-scoped, scoped to this event so Stop and SubagentStop
		// rules don't share state.
		if db != nil && isDedupedSessionScoped(db, rule, sessionID, event) {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				EventName:  event,
				Path:       digest,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: "dedupe",
				LatencyMS:  time.Since(start).Milliseconds(),
			})
			continue
		}

		body := strings.TrimSpace(rule.Inject)
		if len(body) > rule.Budget {
			body = body[:rule.Budget]
		}
		if len(body) > budgetRemaining {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				EventName:  event,
				Path:       digest,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: "budget",
				LatencyMS:  time.Since(start).Milliseconds(),
			})
			continue
		}

		injections = append(injections, fmt.Sprintf("[rule:%s] %s", rule.ID, body))
		budgetUsedTotal += len(body)
		budgetRemaining -= len(body)

		writeToolHookAudit(db, toolHookAuditRow{
			Timestamp:     time.Now().Format(time.RFC3339),
			SessionID:     sessionID,
			CWD:           in.CWD,
			EventName:     event,
			Path:          digest,
			RuleID:        rule.ID,
			Injected:      true,
			InjectionSize: len(body),
			BudgetUsed:    budgetUsedTotal,
			LatencyMS:     time.Since(start).Milliseconds(),
		})
	}

	if !audited && db != nil {
		writeToolHookAudit(db, toolHookAuditRow{
			Timestamp: time.Now().Format(time.RFC3339),
			SessionID: sessionID,
			CWD:       in.CWD,
			EventName: event,
			Path:      digest,
			LatencyMS: time.Since(start).Milliseconds(),
		})
	}

	emitInjections(event, injections)
}

// runPostToolUseHook handles tool + response-regex matching. Fires after a
// tool call returns — ideal for reflex messages on specific error patterns
// (e.g. "Unknown skill" from the Skill tool).
//
// Matching: rule must list PostToolUse in events, match tool name, and (if
// MatchResponse is set) have at least one regex match against the raw
// tool_response JSON bytes. If MatchResponse is empty, the rule fires on
// every matching tool call. Uses the same path-based dedupe as PreToolUse
// (per_file treats the tool name as the "path" surrogate).
func runPostToolUseHook(in ToolHookInput, cfg *ToolHookConfig, db *sql.DB) {
	start := time.Now()
	sessionID := in.SessionID
	path := extractToolPath(in.ToolName, in.ToolInput)
	// Fall back to tool name as the dedupe key when no path is extractable
	// (e.g. Skill tool — the "path" is really the skill name).
	dedupeKey := path
	if dedupeKey == "" {
		dedupeKey = in.ToolName
	}

	audited := false
	budgetRemaining := cfg.Budget
	var injections []string
	budgetUsedTotal := 0

	// Drain pending PostAssistantMsg content hook injections (same as PreToolUse).
	if pending := drainContentHookPending(db, sessionID); len(pending) > 0 {
		injections = append(injections, pending...)
		for _, p := range pending {
			budgetUsedTotal += len(p)
			budgetRemaining -= len(p)
		}
	}

	for _, rule := range cfg.Rules {
		if rule.Disabled || rule.ID == "" {
			continue
		}
		if isTempDisabled(&rule) {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				EventName:  HookEventPostToolUse,
				ToolName:   in.ToolName,
				Path:       dedupeKey,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: tempDisableReason(&rule),
				LatencyMS:  time.Since(start).Milliseconds(),
			})
			continue
		}
		if !rule.eventMatches(HookEventPostToolUse) {
			continue
		}
		if !rule.toolMatches(in.ToolName) {
			continue
		}
		// Filters (AND): match (path glob) / match_input (input regex) /
		// match_response (response regex). At least ONE of the three must be set
		// — otherwise a rule of e.g. `tools: [Bash]` alone would fire on every
		// Bash call, which is almost never the intent. Each filter is optional;
		// when set it must match.
		hasPathMatch := len(rule.Match) > 0
		hasInputMatch := len(rule.MatchInput) > 0
		hasResponseMatch := len(rule.MatchResponse) > 0
		if !hasPathMatch && !hasInputMatch && !hasResponseMatch {
			continue
		}
		if hasPathMatch && !matchesGlob(path, rule.Match) {
			continue
		}
		if hasInputMatch && !matchesInputRegex(in.ToolInput, rule.MatchInput) {
			continue
		}
		if hasResponseMatch && !matchesResponseRegex(in.ToolResponse, rule.MatchResponse) {
			continue
		}
		audited = true

		// skip_input — mirrors PreToolUse semantics. Suppresses the rule when the
		// tool_input matches any skip pattern, even if match_input/match_response
		// already matched. Used to exclude self-referential cases like
		// `echo '... command ...'` or `weiran tool-hook` smoke tests.
		if matchesInputRegex(in.ToolInput, rule.SkipInput) {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				EventName:  HookEventPostToolUse,
				ToolName:   in.ToolName,
				Path:       dedupeKey,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: "skip_input",
				LatencyMS:  time.Since(start).Milliseconds(),
			})
			continue
		}

		if db != nil && isDeduped(db, rule, sessionID, dedupeKey) {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				EventName:  HookEventPostToolUse,
				ToolName:   in.ToolName,
				Path:       dedupeKey,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: "dedupe",
				LatencyMS:  time.Since(start).Milliseconds(),
			})
			continue
		}

		// Side-effect action runs independently of inject — actions like
		// record_fragment_load must fire regardless of whether the rule
		// produces a system-reminder, so we dispatch BEFORE the budget /
		// inject path. Action failures log to stderr but never block.
		if rule.Action != "" {
			runRuleAction(rule.Action, rule.ID, in)
		}

		body := strings.TrimSpace(rule.Inject)
		if len(body) > rule.Budget {
			body = body[:rule.Budget]
		}
		if len(body) > budgetRemaining {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				EventName:  HookEventPostToolUse,
				ToolName:   in.ToolName,
				Path:       dedupeKey,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: "budget",
				LatencyMS:  time.Since(start).Milliseconds(),
			})
			continue
		}

		injections = append(injections, fmt.Sprintf("[rule:%s] %s", rule.ID, body))
		budgetUsedTotal += len(body)
		budgetRemaining -= len(body)

		writeToolHookAudit(db, toolHookAuditRow{
			Timestamp:     time.Now().Format(time.RFC3339),
			SessionID:     sessionID,
			CWD:           in.CWD,
			EventName:     HookEventPostToolUse,
			ToolName:      in.ToolName,
			Path:          dedupeKey,
			RuleID:        rule.ID,
			Injected:      true,
			InjectionSize: len(body),
			BudgetUsed:    budgetUsedTotal,
			LatencyMS:     time.Since(start).Milliseconds(),
		})
	}

	if !audited && db != nil {
		writeToolHookAudit(db, toolHookAuditRow{
			Timestamp: time.Now().Format(time.RFC3339),
			SessionID: sessionID,
			CWD:       in.CWD,
			EventName: HookEventPostToolUse,
			ToolName:  in.ToolName,
			Path:      path,
			LatencyMS: time.Since(start).Milliseconds(),
		})
	}

	emitInjections(HookEventPostToolUse, injections)
}

// isDedupedSessionScoped is the session-scoped dedupe check (no path surrogate).
// Used by UserPromptSubmit, Stop, SubagentStop — events that have a session
// but no per-call path. Caller passes the event name so dedupe scopes the
// (rule, session, event) tuple correctly: a Stop and a UserPromptSubmit rule
// with the same ID would otherwise step on each other's dedupe state.
// "never" → always allow; anything else → 1 per (rule, session, event).
func isDedupedSessionScoped(db *sql.DB, rule ToolHookRule, sessionID, event string) bool {
	if rule.Dedupe == "never" {
		return false
	}
	var n int
	db.QueryRow(`SELECT 1 FROM tool_hook_audit
		WHERE rule_id=? AND session_id=? AND event_name=? AND injected=1 LIMIT 1`,
		rule.ID, sessionID, event).Scan(&n)
	return n == 1
}

// promptDigest returns a short excerpt of the prompt for audit-table observability.
// Kept tiny; the full prompt lives in Claude Code's own session JSONL.
func promptDigest(prompt string, n int) string {
	p := strings.ReplaceAll(strings.ReplaceAll(prompt, "\n", " "), "\r", " ")
	if len([]rune(p)) <= n {
		return p
	}
	runes := []rune(p)
	return string(runes[:n]) + "…"
}

// emitInjections writes the final JSON output that Claude Code consumes.
// Nothing is written when there are no injections.
func emitInjections(event string, injections []string) {
	if len(injections) == 0 {
		return
	}
	body := strings.Join(injections, "\n\n")
	out := map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName":     event,
			"additionalContext": body,
		},
		"suppressOutput": true,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.Encode(out)
}

// emitDecision writes a PreToolUse permission decision — Claude Code will
// short-circuit the tool call based on the verdict (deny / ask / allow).
// Reason is shown to the user in the tool-denied UI and back to the model.
// Only valid for PreToolUse; callers must ensure decision ∈ {deny, ask, allow}.
func emitDecision(event, decision, reason string) {
	out := map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName":            event,
			"permissionDecision":       decision,
			"permissionDecisionReason": reason,
		},
		"suppressOutput": true,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.Encode(out)
}

// ── CLI: `weiran tool-hook stats` ──

// toolHookStats aggregates recent audit rows for diagnostics.
type toolHookStats struct {
	TotalCalls      int                      `json:"total_calls"`
	MatchedCalls    int                      `json:"matched_calls"`
	InjectedCalls   int                      `json:"injected_calls"`
	ByRule          map[string]toolHookRuleS `json:"by_rule"`
	ByTool          map[string]int           `json:"by_tool"`
	ByEvent         map[string]int           `json:"by_event"`
	TopPaths        []toolHookPathStat       `json:"top_paths"`
	SkipBreakdown   map[string]int           `json:"skip_breakdown"`
	DaysQueried     int                      `json:"days_queried"`
	AvgLatencyMS    int64                    `json:"avg_latency_ms"`
}

type toolHookRuleS struct {
	Calls        int   `json:"calls"`
	Injected     int   `json:"injected"`
	AvgInjection int   `json:"avg_injection_bytes"`
	AvgLatencyMS int64 `json:"avg_latency_ms"`
}

type toolHookPathStat struct {
	Path  string `json:"path"`
	Count int    `json:"count"`
}

// queryToolHookStats aggregates the last N days.
func queryToolHookStats(db *sql.DB, days int) (*toolHookStats, error) {
	since := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)
	stats := &toolHookStats{
		ByRule:        map[string]toolHookRuleS{},
		ByTool:        map[string]int{},
		ByEvent:       map[string]int{},
		SkipBreakdown: map[string]int{},
		DaysQueried:   days,
	}

	// totals + latency
	row := db.QueryRow(`SELECT COUNT(*), SUM(injected), ROUND(AVG(latency_ms))
		FROM tool_hook_audit WHERE timestamp >= ?`, since)
	var latency sql.NullFloat64
	var injected sql.NullInt64
	row.Scan(&stats.TotalCalls, &injected, &latency)
	stats.InjectedCalls = int(injected.Int64)
	stats.AvgLatencyMS = int64(latency.Float64)

	// matched (rule_id != '')
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit
		WHERE timestamp >= ? AND rule_id != ''`, since).Scan(&stats.MatchedCalls)

	// by rule
	rows, err := db.Query(`SELECT rule_id, COUNT(*), SUM(injected),
		ROUND(AVG(CASE WHEN injected=1 THEN injection_size ELSE 0 END)),
		ROUND(AVG(latency_ms))
		FROM tool_hook_audit WHERE timestamp >= ? AND rule_id != ''
		GROUP BY rule_id ORDER BY COUNT(*) DESC`, since)
	if err == nil {
		for rows.Next() {
			var id string
			var s toolHookRuleS
			var injSize, lat sql.NullFloat64
			rows.Scan(&id, &s.Calls, &s.Injected, &injSize, &lat)
			s.AvgInjection = int(injSize.Float64)
			s.AvgLatencyMS = int64(lat.Float64)
			stats.ByRule[id] = s
		}
		rows.Close()
	}

	// by tool
	rows, err = db.Query(`SELECT tool_name, COUNT(*) FROM tool_hook_audit
		WHERE timestamp >= ? GROUP BY tool_name`, since)
	if err == nil {
		for rows.Next() {
			var t string
			var n int
			rows.Scan(&t, &n)
			stats.ByTool[t] = n
		}
		rows.Close()
	}

	// by event
	rows, err = db.Query(`SELECT event_name, COUNT(*) FROM tool_hook_audit
		WHERE timestamp >= ? GROUP BY event_name`, since)
	if err == nil {
		for rows.Next() {
			var e string
			var n int
			rows.Scan(&e, &n)
			stats.ByEvent[e] = n
		}
		rows.Close()
	}

	// top paths
	rows, err = db.Query(`SELECT path, COUNT(*) FROM tool_hook_audit
		WHERE timestamp >= ? AND path != '' GROUP BY path ORDER BY COUNT(*) DESC LIMIT 15`, since)
	if err == nil {
		for rows.Next() {
			var p toolHookPathStat
			rows.Scan(&p.Path, &p.Count)
			stats.TopPaths = append(stats.TopPaths, p)
		}
		rows.Close()
	}

	// skip breakdown
	rows, err = db.Query(`SELECT skip_reason, COUNT(*) FROM tool_hook_audit
		WHERE timestamp >= ? AND skip_reason != '' GROUP BY skip_reason`, since)
	if err == nil {
		for rows.Next() {
			var r string
			var n int
			rows.Scan(&r, &n)
			stats.SkipBreakdown[r] = n
		}
		rows.Close()
	}

	return stats, nil
}

// handleToolHook dispatches the `tool-hook` subcommand.
func handleToolHook(args []string) {
	if len(args) == 0 {
		// no subcommand = run as hook (stdin → stdout injection)
		runToolHook()
		return
	}
	switch args[0] {
	case "stats":
		handleToolHookStats(args[1:])
	case "test":
		handleToolHookTest(args[1:])
	case "rules":
		handleToolHookRules()
	case "events":
		handleToolHookEvents()
	case "gc":
		handleToolHookGC(args[1:])
	case "log":
		handleToolHookLog(args[1:])
	case "disable":
		handleToolHookDisable(args[1:])
	case "enable":
		handleToolHookEnable(args[1:])
	case "disables":
		handleToolHookDisables(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "usage: %s tool-hook <subcommand>\n\n", appName)
		fmt.Fprintf(os.Stderr, "  (no args)                run as hook (reads JSON from stdin)\n")
		fmt.Fprintf(os.Stderr, "  stats  [--days N] [--json]\n                           aggregated audit stats (human by default)\n")
		fmt.Fprintf(os.Stderr, "  log    [filters…]        tail recent audit rows (see log --help)\n")
		fmt.Fprintf(os.Stderr, "  test   <tool> <path>     dry-run PreToolUse rule matching\n")
		fmt.Fprintf(os.Stderr, "  rules                    list configured rules (YAML)\n")
		fmt.Fprintf(os.Stderr, "  events                   list hook events this binary recognizes\n")
		fmt.Fprintf(os.Stderr, "  gc     [--days N]        delete audit rows older than N days (default 30)\n")
		fmt.Fprintf(os.Stderr, "  disable <rule> --until <spec> [--reason \"…\"]\n                           temp disable (required --until, 2h cap)\n")
		fmt.Fprintf(os.Stderr, "  enable  <rule>           clear temp disable early\n")
		fmt.Fprintf(os.Stderr, "  disables                 list active + expired temp disables\n")
		os.Exit(1)
	}
}

// handleToolHookLog dumps recent audit rows with filtering. Output format is
// a compact single-line-per-row KV pairs which is greppable and fits terminal.
//
// Filters (combinable, AND semantics):
//   --event    <name>     PreToolUse / UserPromptSubmit / …
//   --session  <id>       exact session id match
//   --rule     <id>       only rows where this rule fired or skipped
//   --tool     <name>     only PreToolUse/PostToolUse rows for this tool
//   --injected             only rows that actually injected
//   --skipped              only rows with a skip_reason (dedupe/budget)
//   --grep     <substr>   substring match against path/prompt digest
//   --days     <N>        lookback window (default 1)
//   --limit    <N>        row cap (default 50, max 1000)
//   --json                emit NDJSON instead of human format
//   --help                usage
func handleToolHookLog(args []string) {
	days := 1
	limit := 50
	var filterEvent, filterSession, filterRule, filterTool, filterGrep string
	jsonOut := false
	onlyInjected := false
	onlySkipped := false

	for i := 0; i < len(args); i++ {
		a := args[i]
		peek := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch a {
		case "--help", "-h":
			fmt.Print(`weiran tool-hook log — search the audit table.

Filters (all combinable):
  --event <name>     PreToolUse | PostToolUse | UserPromptSubmit | Stop | …
  --session <id>     exact session id
  --rule <id>        rule id (match or skip)
  --tool <name>      Read | Edit | Write | Bash | …
  --injected         only rows that actually injected context
  --skipped          only rows with a skip_reason
  --grep <substr>    substring match against path/prompt digest
  --days <N>         lookback window (default 1)
  --limit <N>        row cap (default 50, max 1000)
  --json             NDJSON output instead of human format

Examples:
  weiran tool-hook log --event UserPromptSubmit --days 7
  weiran tool-hook log --rule remember_signal --injected
  weiran tool-hook log --grep memory/topics/feedback --limit 100
  weiran tool-hook log --tool Edit --days 3 --json | jq .
`)
			return
		case "--event":
			filterEvent = peek()
		case "--session":
			filterSession = peek()
		case "--rule":
			filterRule = peek()
		case "--tool":
			filterTool = peek()
		case "--grep":
			filterGrep = peek()
		case "--days":
			fmt.Sscanf(peek(), "%d", &days)
		case "--limit":
			fmt.Sscanf(peek(), "%d", &limit)
		case "--json":
			jsonOut = true
		case "--injected":
			onlyInjected = true
		case "--skipped":
			onlySkipped = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s (try --help)\n", a)
			os.Exit(1)
		}
	}
	if days < 1 {
		days = 1
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 1000 {
		limit = 1000
	}

	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "open DB: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// Build parameterized query to keep grep noise minimal.
	since := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)
	q := `SELECT timestamp, session_id, event_name, tool_name, path, rule_id,
		injected, skip_reason, injection_size, budget_used, latency_ms
		FROM tool_hook_audit WHERE timestamp >= ?`
	params := []interface{}{since}
	if filterEvent != "" {
		q += " AND event_name=?"
		params = append(params, filterEvent)
	}
	if filterSession != "" {
		q += " AND session_id=?"
		params = append(params, filterSession)
	}
	if filterRule != "" {
		q += " AND rule_id=?"
		params = append(params, filterRule)
	}
	if filterTool != "" {
		q += " AND tool_name=?"
		params = append(params, filterTool)
	}
	if onlyInjected {
		q += " AND injected=1"
	}
	if onlySkipped {
		q += " AND skip_reason != ''"
	}
	if filterGrep != "" {
		q += " AND path LIKE ?"
		params = append(params, "%"+filterGrep+"%")
	}
	q += " ORDER BY id DESC LIMIT ?"
	params = append(params, limit)

	rows, err := db.Query(q, params...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	type logRow struct {
		Timestamp     string `json:"timestamp"`
		SessionID     string `json:"session_id"`
		EventName     string `json:"event_name"`
		ToolName      string `json:"tool_name"`
		Path          string `json:"path"`
		RuleID        string `json:"rule_id"`
		Injected      bool   `json:"injected"`
		SkipReason    string `json:"skip_reason"`
		InjectionSize int    `json:"injection_size"`
		BudgetUsed    int    `json:"budget_used"`
		LatencyMS     int64  `json:"latency_ms"`
	}
	var out []logRow
	for rows.Next() {
		var r logRow
		var inj int
		if err := rows.Scan(&r.Timestamp, &r.SessionID, &r.EventName, &r.ToolName,
			&r.Path, &r.RuleID, &inj, &r.SkipReason, &r.InjectionSize,
			&r.BudgetUsed, &r.LatencyMS); err != nil {
			continue
		}
		r.Injected = inj == 1
		out = append(out, r)
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		for _, r := range out {
			enc.Encode(r)
		}
		return
	}

	if len(out) == 0 {
		fmt.Fprintf(os.Stderr, "no rows matched (try --days %d --limit %d or relax filters)\n", days, limit)
		return
	}

	// Human output: rows newest-first. Each line is a stable KV sequence so
	// `grep event=UserPromptSubmit` / `awk '$0 ~ /rule=remember/'` still work.
	for _, r := range out {
		verdict := "observe"
		if r.Injected {
			verdict = "INJECT"
		} else if r.SkipReason != "" {
			verdict = "skip:" + r.SkipReason
		}
		line := fmt.Sprintf("%s  event=%-17s tool=%-10s rule=%-30s verdict=%-15s sess=%s",
			r.Timestamp, r.EventName, nz(r.ToolName), nz(r.RuleID), verdict, shortSessionID(r.SessionID, 8))
		if r.Path != "" {
			line += "  path=" + r.Path
		}
		if r.Injected || r.InjectionSize > 0 {
			line += fmt.Sprintf("  bytes=%d", r.InjectionSize)
		}
		fmt.Println(line)
	}
	fmt.Fprintf(os.Stderr, "\n(%d rows in last %dd; --limit %d, --days %d)\n", len(out), days, limit, days)
}

// nz returns "-" when s is empty, keeping log columns aligned.
func nz(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// shortSessionID truncates a UUID-ish session id for visual density. Named
// explicitly to avoid colliding with the workspace-aware `shortID` in safe.go.
func shortSessionID(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// handleToolHookEvents prints the list of hook events this binary recognizes.
// Useful when wiring up ~/.claude/settings.json: any event printed here can
// be safely registered; anything else will be audit-only with "unknown event".
func handleToolHookEvents() {
	fmt.Printf("%d hook events recognized (per Claude Code settings.json allowlist):\n\n", len(AllHookEvents))
	for _, e := range AllHookEvents {
		matcher := "audit-only"
		switch e {
		case HookEventPreToolUse:
			matcher = "rule-driven (tools + match glob)"
		case HookEventPostToolUse:
			matcher = "rule-driven (tools + match_response/match_input regex)"
		case HookEventUserPromptSubmit:
			matcher = "rule-driven (match_prompt regex)"
		case HookEventStop, HookEventSubagentStop:
			matcher = "rule-driven (events list, no other match needed)"
		}
		fmt.Printf("  %-20s %s\n", e, matcher)
	}
	fmt.Println("\nServer-side events (evaluated by weiran server, not CC hook binary):")
	fmt.Printf("  %-20s %s\n", HookEventPostAssistantMsg, "rule-driven (match_prompt on assistant text, server writes → tool-hook drains)")
	fmt.Println("\nEvents not listed above are still accepted — their event_name is recorded verbatim in the audit table, flagged as unknown.")
}

// handleToolHookGC deletes audit rows older than `days`. Defaults to 30 days.
// Safe to run concurrently (DELETE is atomic). Reports how many rows were
// removed and the resulting table row count.
func handleToolHookGC(args []string) {
	days := 30
	for i, a := range args {
		if a == "--days" && i+1 < len(args) {
			fmt.Sscanf(args[i+1], "%d", &days)
		}
	}
	if days < 1 {
		fmt.Fprintf(os.Stderr, "refusing to gc with --days < 1 (would delete everything)\n")
		os.Exit(1)
	}
	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "open DB: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	cutoff := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)
	res, err := db.Exec(`DELETE FROM tool_hook_audit WHERE timestamp < ?`, cutoff)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gc failed: %v\n", err)
		os.Exit(1)
	}
	deleted, _ := res.RowsAffected()

	var remaining int
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit`).Scan(&remaining)

	// VACUUM is cheap after a big DELETE and keeps the sqlite file from
	// growing unbounded. Best-effort; ignore errors.
	_, _ = db.Exec(`VACUUM`)

	fmt.Printf("deleted %d rows older than %d days (cutoff=%s); %d rows remaining\n",
		deleted, days, cutoff, remaining)
}

func handleToolHookStats(args []string) {
	days := 7
	jsonOut := false
	for i, a := range args {
		switch a {
		case "--days":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &days)
			}
		case "--json":
			jsonOut = true
		}
	}
	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "open DB: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	stats, err := queryToolHookStats(db, days)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		os.Exit(1)
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(stats)
		return
	}
	printToolHookStatsHuman(stats)
}

// printToolHookStatsHuman is the default stats output — aligned columns
// optimized for reading and grep-ability. Key lines start with a stable
// prefix (`rule=`, `event=`, `tool=`, `path=`) so `grep` and `awk` work.
func printToolHookStatsHuman(s *toolHookStats) {
	fmt.Printf("══ tool-hook stats (last %d days) ══\n", s.DaysQueried)
	fmt.Printf("calls=%d  matched=%d  injected=%d  avg_latency_ms=%d\n\n",
		s.TotalCalls, s.MatchedCalls, s.InjectedCalls, s.AvgLatencyMS)

	if len(s.ByEvent) > 0 {
		fmt.Println("── by event ──")
		keys := sortedStringKeys(s.ByEvent)
		for _, k := range keys {
			fmt.Printf("event=%-20s calls=%d\n", k, s.ByEvent[k])
		}
		fmt.Println()
	}

	if len(s.ByRule) > 0 {
		fmt.Println("── by rule ──")
		// Sort by calls descending for visual ranking.
		type kv struct {
			k string
			v toolHookRuleS
		}
		list := make([]kv, 0, len(s.ByRule))
		for k, v := range s.ByRule {
			list = append(list, kv{k, v})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].v.Calls > list[j].v.Calls })
		for _, e := range list {
			fmt.Printf("rule=%-36s calls=%-5d injected=%-5d avg_bytes=%-5d avg_latency_ms=%d\n",
				e.k, e.v.Calls, e.v.Injected, e.v.AvgInjection, e.v.AvgLatencyMS)
		}
		fmt.Println()
	}

	if len(s.ByTool) > 0 {
		fmt.Println("── by tool (PreToolUse/PostToolUse) ──")
		keys := sortedStringKeys(s.ByTool)
		for _, k := range keys {
			label := k
			if label == "" {
				label = "<none>"
			}
			fmt.Printf("tool=%-14s calls=%d\n", label, s.ByTool[k])
		}
		fmt.Println()
	}

	if len(s.TopPaths) > 0 {
		fmt.Println("── top paths ──")
		for _, p := range s.TopPaths {
			fmt.Printf("path=%-80s calls=%d\n", trimForDisplay(p.Path, 80), p.Count)
		}
		fmt.Println()
	}

	if len(s.SkipBreakdown) > 0 {
		fmt.Println("── skip reasons ──")
		keys := sortedStringKeys(s.SkipBreakdown)
		for _, k := range keys {
			fmt.Printf("skip=%-10s calls=%d\n", k, s.SkipBreakdown[k])
		}
	}
}

func sortedStringKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func handleToolHookTest(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s tool-hook test <tool_name> <path>\n", appName)
		os.Exit(1)
	}
	tool, path := args[0], args[1]
	cfg, err := loadToolHookConfig(defaultToolHookConfigPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	hit := 0
	for _, rule := range cfg.Rules {
		if rule.Disabled {
			continue
		}
		if !rule.toolMatches(tool) {
			continue
		}
		if !matchesGlob(path, rule.Match) {
			continue
		}
		hit++
		fmt.Printf("✓ match  %s  (priority=%d, budget=%d, dedupe=%s)\n",
			rule.ID, rule.Priority, rule.Budget, rule.Dedupe)
		fmt.Printf("         inject: %s\n", trimForDisplay(rule.Inject, 100))
	}
	if hit == 0 {
		fmt.Printf("no match for tool=%s path=%s\n", tool, path)
	}
}

func handleToolHookRules() {
	cfg, err := loadToolHookConfig(defaultToolHookConfigPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("config: %s\n", defaultToolHookConfigPath())
	fmt.Printf("global budget: %d chars\n", cfg.Budget)
	fmt.Printf("%d rules:\n\n", len(cfg.Rules))
	for _, r := range cfg.Rules {
		status := "enabled"
		if r.Disabled {
			status = "DISABLED"
		}
		fmt.Printf("• %s [%s] priority=%d budget=%d dedupe=%s\n",
			r.ID, status, r.Priority, r.Budget, r.Dedupe)
		fmt.Printf("    tools: %v\n", r.Tools)
		fmt.Printf("    match: %v\n", r.Match)
		if len(r.MatchInput) > 0 {
			fmt.Printf("    match_input: %v\n", r.MatchInput)
		}
		if len(r.SkipInput) > 0 {
			fmt.Printf("    skip_input: %v\n", r.SkipInput)
		}
		if len(r.MatchPrompt) > 0 {
			fmt.Printf("    match_prompt: %v\n", r.MatchPrompt)
		}
		if len(r.MatchResponse) > 0 {
			fmt.Printf("    match_response: %v\n", r.MatchResponse)
		}
		if r.Decision != "" {
			fmt.Printf("    decision: %s\n", r.Decision)
		}
		fmt.Printf("    inject: %s\n\n", trimForDisplay(r.Inject, 120))
	}
}

func trimForDisplay(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ── In-process evaluator (Round 4: codex backend approval bridge) ──
//
// evaluateToolHookForApproval runs the PreToolUse rule chain on a synthesized
// ToolHookInput and returns the matching decision in-process. Used by
// server_process_codex.go (attachCodexBridge → handleCodexApproval) to route
// codex's */requestApproval through the same rule set CC's PreToolUse hook
// does. CC continues to use the subprocess hook handler (runPreToolUseHook
// + emitDecision) — this is a parallel path, not a replacement.
//
// Returns:
//
//	decision = "deny" | "ask" | "allow" | ""   (empty = no rule fired)
//	reason   = the rule's Inject body (truncated to its budget) for the
//	           first decision rule that fired, or the concatenated injects
//	           from non-decision rules when no decision rule matched
//	contexts = ordered list of "[rule:<id>] <inject body>" strings for
//	           non-decision rules whose injections would have been
//	           additionalContext under the subprocess path. The codex
//	           bridge surfaces these on a codex_hook_decision SSE for
//	           observability; they don't change the allow/deny outcome.
//
// Budget / dedupe behavior matches runPreToolUseHook so the two paths can't
// diverge silently. Audit rows are NOT written here — the codex bridge
// writes its own SSE event for visibility, and the subprocess path remains
// the canonical source of audit data for CC sessions. (We could add audit
// here but Round 4 keeps schema impact minimal.)
//
// Decision precedence: rules are evaluated in YAML order. The first rule
// with a non-empty Decision short-circuits — same as runPreToolUseHook.
// Rules without Decision contribute their Inject body to contexts and
// continue.
func evaluateToolHookForApproval(in ToolHookInput) (decision, reason string, contexts []string) {
	cfgPath := defaultToolHookConfigPath()
	cfg, err := loadToolHookConfig(cfgPath)
	if err != nil {
		// Config missing or malformed → no policy → caller's default applies.
		return "", "", nil
	}

	path := extractToolPath(in.ToolName, in.ToolInput)
	budgetRemaining := cfg.Budget
	if budgetRemaining <= 0 {
		budgetRemaining = 1500
	}

	for _, rule := range cfg.Rules {
		if rule.Disabled || rule.ID == "" {
			continue
		}
		if isTempDisabled(&rule) {
			continue
		}
		if !rule.eventMatches(HookEventPreToolUse) {
			continue
		}
		if !rule.toolMatches(in.ToolName) {
			continue
		}
		hasPathMatch := len(rule.Match) > 0
		hasInputMatch := len(rule.MatchInput) > 0
		if !hasPathMatch && !hasInputMatch {
			continue
		}
		if hasPathMatch && !matchesGlob(path, rule.Match) {
			continue
		}
		if hasInputMatch && !matchesInputRegex(in.ToolInput, rule.MatchInput) {
			continue
		}
		if matchesInputRegex(in.ToolInput, rule.SkipInput) {
			continue
		}

		body := strings.TrimSpace(rule.Inject)
		ruleBudget := rule.Budget
		if ruleBudget <= 0 {
			ruleBudget = 500
		}
		if len(body) > ruleBudget {
			body = body[:ruleBudget]
		}

		if rule.Decision != "" {
			if !validDecision(rule.Decision) {
				continue
			}
			if len(body) > budgetRemaining {
				continue
			}
			return rule.Decision, body, contexts
		}

		if len(body) > budgetRemaining {
			continue
		}
		contexts = append(contexts, fmt.Sprintf("[rule:%s] %s", rule.ID, body))
		budgetRemaining -= len(body)
	}

	// No decision rule fired → caller (handleCodexApproval) defaults to allow.
	// Surface the contexts so the bridge can log them for observability.
	if len(contexts) > 0 {
		reason = strings.Join(contexts, "\n")
	}
	return "", reason, contexts
}
