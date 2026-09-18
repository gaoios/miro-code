package app

// prompt_routing.go — 人格技能化 (lazy-load persona router)
//
// Concept: SOUL.md is no longer a monolithic 3000-line document loaded into
// every session. Instead, it's a directory of fragment markdown files (under
// workspace/soul/), each tagged with `modes: [...]` in YAML frontmatter
// declaring which soul-modes load it. At session startup, detectSoulMode()
// inspects (CLI mode, launch directory, source, first message) to pick a
// soul-mode, then prompt assembly loads only the fragments matching that mode.
//
// Phase A (this file): routing infrastructure + audit. No behavior change yet.
// All soul-modes initially load the same fragments as before, so byte-output
// stays identical to the pre-router prompt. We only start *recording* what
// would have been chosen.
//
// Phase B will introduce fragment files under soul/ and a fragment loader.
// Phase C will activate per-mode profile differentiation.
// Phase D will remove the legacy monolithic SOUL.md.
//
// Cache friendliness:
//
//   The Anthropic API prompt cache uses byte-prefix matching with a 5-minute
//   TTL. To maximize cache hits across sessions, fragment ordering inside each
//   mode profile is hard-coded (not dynamic by frontmatter priority) and modes
//   form a nesting hierarchy:
//
//     core ⊂ ops
//     core ⊂ technical
//     core ⊂ emotional ⊂ intimate
//     core ⊂ evolve (= all)
//
//   This guarantees that, e.g., an `intimate` session re-uses the cached
//   prefix produced by an earlier `emotional` session, since `emotional`'s
//   fragments are a strict prefix of `intimate`'s.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// SoulMode is the persona-routing axis, orthogonal to currentMode (the CLI
// operation mode). One CLI mode can map to multiple soul modes (e.g.
// `interactive` may resolve to emotional/intimate/technical depending on
// signals).
type SoulMode string

const (
	SoulModeCore      SoulMode = "core"      // baseline, always loaded as a subset
	SoulModeOps       SoulMode = "ops"       // heartbeat / cron — minimal, ops-focused
	SoulModeTechnical SoulMode = "technical" // engineering work, no persona warmth
	SoulModeEmotional SoulMode = "emotional" // chat / TG / WebUI default — warm but not explicit
	SoulModeIntimate  SoulMode = "intimate"  // private/explicit — full body + persona
	SoulModeEvolve    SoulMode = "evolve"    // self-evolution — load everything for full self-awareness
)

// ValidSoulModes is the canonical list. Used by `weiran lint` to validate
// fragment frontmatter `modes: [...]` fields.
var ValidSoulModes = []SoulMode{
	SoulModeCore,
	SoulModeOps,
	SoulModeTechnical,
	SoulModeEmotional,
	SoulModeIntimate,
	SoulModeEvolve,
}

// IsValidSoulMode returns true if s is one of the canonical mode names.
func IsValidSoulMode(s string) bool {
	for _, m := range ValidSoulModes {
		if string(m) == s {
			return true
		}
	}
	return false
}

// FragmentFrontmatterSpec describes the required YAML frontmatter for a soul
// fragment file (under workspace/soul/**.md). `weiran lint` enforces this.
//
// Required:
//   - id: stable identifier, must match filename stem
//   - title: human-readable
//   - modes: non-empty list of valid SoulMode names
//
// Optional:
//   - priority: int,装配排序 tie-breaker (default = parsed from filename prefix like "12-")
//   - tokens_est: int, estimated token count
//   - description: short one-liner
type FragmentFrontmatterSpec struct {
	RequiredFields []string
	OptionalFields []string
}

var fragmentFrontmatterSpec = FragmentFrontmatterSpec{
	RequiredFields: []string{"id", "title", "modes"},
	OptionalFields: []string{"priority", "tokens_est", "description", "tags"},
}

// FragmentMeta is the metadata extracted from a fragment file's frontmatter,
// used by the lazy-load index to tell the agent what fragments exist and
// (via tags + description) when each one is relevant.
type FragmentMeta struct {
	ID          string
	Title       string
	Description string
	Tags        []string
	Modes       []string
	Path        string // absolute path the agent uses to Read it
}

// readFragmentMeta parses one fragment file's frontmatter into a FragmentMeta.
// Returns nil if the file lacks frontmatter or required fields.
func readFragmentMeta(path string) *FragmentMeta {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	fm := parseMdFrontmatter(string(data))
	if fm == nil {
		return nil
	}
	return &FragmentMeta{
		ID:          fm["id"],
		Title:       fm["title"],
		Description: fm["description"],
		Tags:        parseModesList(fm["tags"]), // same [a, b, c] shape as modes
		Modes:       parseModesList(fm["modes"]),
		Path:        path,
	}
}

// listAllFragmentMetas walks soulDir and returns FragmentMeta for every
// fragment with a parseable frontmatter, sorted by numeric filename prefix
// (same ordering as loadFragmentsByMode for cache-stability).
func listAllFragmentMetas(soulDir string) ([]*FragmentMeta, error) {
	var out []*FragmentMeta
	err := filepath.Walk(soulDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		meta := readFragmentMeta(path)
		if meta == nil || meta.ID == "" {
			return nil
		}
		out = append(out, meta)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		ki := extractFragmentSortKey(out[i].Path)
		kj := extractFragmentSortKey(out[j].Path)
		if ki != kj {
			return ki < kj
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// fragmentHasMode reports whether m is in meta.Modes.
func (m *FragmentMeta) fragmentHasMode(target SoulMode) bool {
	for _, name := range m.Modes {
		if name == string(target) {
			return true
		}
	}
	return false
}

// asciiOnlyRegex matches strings made of ASCII letters/digits/underscores only.
// We use it to decide whether a tag should be matched with \b word boundaries
// (Latin) or plain substring (CJK / mixed).
var asciiOnlyRegex = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// matchTagInMessage reports whether tag appears in msgLower. ASCII-only tags
// require word-boundary match (so "master" doesn't fire on "masterclass" or
// "master-essential" — actually we WANT that, but not on every "Master"-cased
// English word; this is the conservative bound). CJK / mixed tags fall back
// to plain substring because RE2 \b doesn't work on non-Latin codepoints.
func matchTagInMessage(tag, msgLower string) bool {
	if tag == "" {
		return false
	}
	tagL := strings.ToLower(tag)
	if asciiOnlyRegex.MatchString(tagL) {
		re, err := regexp.Compile(`\b` + regexp.QuoteMeta(tagL) + `\b`)
		if err != nil {
			return strings.Contains(msgLower, tagL)
		}
		return re.MatchString(msgLower)
	}
	return strings.Contains(msgLower, tagL)
}

// detectTopicFragments scans a user message against every fragment's tag list
// and returns the file paths of fragments whose tags hit the message AND that
// haven't been loaded yet. This is the server-side "fallback" detector — it
// runs alongside the agent-driven Read flow: if the agent fails to Read a
// relevant fragment on its own, this function picks it up and the caller
// patches it in via the soul-patch protocol.
//
// Empty message → nil (no triggers possible).
// Empty allMetas → nil (no fragments to consider).
//
// Order is preserved from allMetas (numeric prefix sort) so cache prefixes
// stay deterministic when the same set of fragments is patched in.
func detectTopicFragments(message string, allMetas []*FragmentMeta, already map[string]bool) []string {
	if message == "" || len(allMetas) == 0 {
		return nil
	}
	msgLower := strings.ToLower(message)
	var hits []string
	for _, m := range allMetas {
		// Skip fragments already in the prefix-loaded set (core / always_load)
		// or already Read by the agent earlier this session.
		if already[m.Path] {
			continue
		}
		// Always-load fragments (core mode) shouldn't be auto-patched as
		// topics — they're already in the prompt prefix.
		if m.fragmentHasMode(SoulModeCore) {
			continue
		}
		for _, tag := range m.Tags {
			if matchTagInMessage(tag, msgLower) {
				hits = append(hits, m.Path)
				break
			}
		}
	}
	return hits
}

// isLazyAssemblyMode returns true for soul modes that should use the
// "core必装 + 其余索引化" layout instead of "load full mode set". These are
// the interactive modes where the agent picks fragments by topic — non-
// interactive modes (cron / heartbeat / evolve) load the full set so they
// have the whole persona available without needing to issue Read calls.
func isLazyAssemblyMode(mode SoulMode) bool {
	switch mode {
	case SoulModeEmotional, SoulModeIntimate, SoulModeTechnical:
		return true
	}
	return false
}

// buildLazyFragmentSection produces the SOUL section body for lazy modes:
//
//   - alwaysContent: concatenated bodies of all fragments tagged into the
//     `core` mode (always_load equivalent — identity, master-essential,
//     protocol). These are NOT skippable.
//   - indexMarkdown: a markdown listing of every NON-core fragment with
//     id / title / description / tags / absolute path. The agent reads
//     this index, decides which fragments are relevant to the current
//     conversation topic, and uses the Read tool to load them. Once Read,
//     the body lives in conversation history (turn-level cache) so the
//     fragment is "loaded once" for the rest of the session.
//
// alwaysPaths is the slice of file paths actually emitted as alwaysContent —
// returned so the audit layer can record what was prefix-loaded.
func buildLazyFragmentSection(soulDir string) (alwaysContent string, alwaysPaths []string, indexMarkdown string, err error) {
	metas, err := listAllFragmentMetas(soulDir)
	if err != nil {
		return "", nil, "", err
	}

	var alwaysMetas, lazyMetas []*FragmentMeta
	for _, m := range metas {
		if m.fragmentHasMode(SoulModeCore) {
			alwaysMetas = append(alwaysMetas, m)
		} else {
			lazyMetas = append(lazyMetas, m)
		}
	}

	// Concatenate always-load bodies (strip frontmatter, preserve order).
	var ab strings.Builder
	for i, meta := range alwaysMetas {
		data, rErr := os.ReadFile(meta.Path)
		if rErr != nil {
			continue
		}
		body := stripFragmentFrontmatter(string(data))
		if i > 0 && ab.Len() > 0 {
			ab.WriteString("\n")
		}
		ab.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			ab.WriteString("\n")
		}
		alwaysPaths = append(alwaysPaths, meta.Path)
	}

	// Build the lazy fragment index.
	//
	// The index has two parts: a universal preamble that the framework owns
	// (mode mechanics — "core fragments are pre-loaded, the rest you Read on
	// demand") and an optional per-agent hint with concrete examples of when
	// to Read which fragments. The hint comes from <workspace>/routing.yaml's
	// `fragment_index_hint` field and is private user data; the framework
	// must not hardcode any agent-specific examples here.
	var ib strings.Builder
	ib.WriteString("## Soul Fragments (lazy-load — 你的人格分布在这些文件里)\n\n")
	ib.WriteString("当前 prompt 只装载了 `[core]` 必要集合（身份 + 主人核心信息 + 协议）。其余 fragment 在你需要时用 **Read 工具**加载——用绝对路径 Read，内容会进入对话上下文，本 session 内已 Read 过的不需要重复加载（在历史里能看到）。\n\n")
	ib.WriteString("**和技能一样**：看 description 和 tags 决定何时该 Read。不是每个 fragment 每次都要装——按当前对话话题选。\n\n")
	if hint := loadRoutingConfig().fragmentIndexHint; hint != "" {
		ib.WriteString(hint)
		if !strings.HasSuffix(hint, "\n") {
			ib.WriteString("\n")
		}
		ib.WriteString("\n")
	}

	for _, m := range lazyMetas {
		// Format: `- title (#tag1 #tag2) — description\n  Read: <abs path>`
		var tagPart string
		if len(m.Tags) > 0 {
			tags := make([]string, len(m.Tags))
			for i, t := range m.Tags {
				tags[i] = "#" + t
			}
			tagPart = " (" + strings.Join(tags, " ") + ")"
		}
		desc := m.Description
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&ib, "- **%s**%s — %s\n  Read: `%s`\n", m.Title, tagPart, desc, m.Path)
	}

	return ab.String(), alwaysPaths, ib.String(), nil
}

// RoutingSignals captures inputs to detectSoulMode for transparency and audit.
type RoutingSignals struct {
	CLIMode      string `json:"cli_mode"`
	LaunchDir    string `json:"launch_dir"`
	Source       string `json:"source"`        // telegram / webui / cron / cli / spawn
	FirstMessage string `json:"first_message"` // truncated, first non-empty user turn if known
	Explicit     string `json:"explicit"`      // env WEIRAN_SOUL_MODE or --soul-mode flag
}

// RoutingConfig is loaded from <workspace>/routing.yaml. It declares
// user-private signals (trigger words, cwd prefixes, source defaults) that
// drive detectSoulMode. The framework code in this file deliberately contains
// no concrete trigger words or directory paths — those are private data and
// must live in the user's workspace, not in soul-cli's open-source codebase.
//
// Schema example (workspace/routing.yaml):
//
//	intimate_triggers:
//	  - '^/<your-trigger-command>\b'
//	  - '^<your-trigger-phrase>'
//	technical_cwd_prefixes:
//	  - '~/some-work-tree'
//	  - '~/another-work-tree'
//	source_defaults:
//	  webui: emotional
//	  telegram-bot: emotional
//	fallback: emotional
//
// All sections are optional. When the file or a section is absent the router
// falls back to SoulModeEmotional, which keeps behavior backward-compatible.
type RoutingConfig struct {
	IntimateTriggers     []string          `yaml:"intimate_triggers"`
	TechnicalCwdPrefixes []string          `yaml:"technical_cwd_prefixes"`
	SourceDefaults       map[string]string `yaml:"source_defaults"`
	Fallback             string            `yaml:"fallback"`
	// FragmentIndexHint is the per-agent guidance line embedded in the lazy
	// fragment index (after the universal "load only what you need" preamble).
	// Used to give the agent agent-specific examples of when to load which
	// fragments. Absent → framework prints only the universal preamble (no
	// per-agent leak into framework code).
	FragmentIndexHint string `yaml:"fragment_index_hint"`
}

// compiledRoutingConfig caches the parsed config + compiled regexes.
type compiledRoutingConfig struct {
	intimate          *regexp.Regexp // nil if no triggers configured
	cwdPrefixes       []string       // already $HOME-expanded
	sources           map[string]string
	fallback          SoulMode
	fragmentIndexHint string // per-agent guidance for the lazy index; "" = no extra hint
}

var (
	routingCfgOnce  sync.Once
	routingCfgValue *compiledRoutingConfig
	routingCfgPath  = "" // overridable in tests
)

// loadRoutingConfig loads and caches the routing config from
// <workspace>/routing.yaml (or routingCfgPath in tests). On any error or
// missing file it returns a config that produces the safe default
// (emotional fallback, no triggers).
func loadRoutingConfig() *compiledRoutingConfig {
	routingCfgOnce.Do(func() {
		routingCfgValue = readRoutingConfig(resolveRoutingConfigPath())
	})
	return routingCfgValue
}

// resetRoutingConfigForTest clears the cached config so tests can re-load
// after pointing routingCfgPath elsewhere. Test-only.
func resetRoutingConfigForTest(path string) {
	routingCfgPath = path
	routingCfgOnce = sync.Once{}
	routingCfgValue = nil
}

func resolveRoutingConfigPath() string {
	if routingCfgPath != "" {
		return routingCfgPath
	}
	return filepath.Join(workspace, "routing.yaml")
}

func readRoutingConfig(path string) *compiledRoutingConfig {
	cc := &compiledRoutingConfig{
		fallback: SoulModeEmotional, // safe default
		sources:  map[string]string{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cc // no file → empty config, emotional fallback
	}
	var rc RoutingConfig
	if err := yaml.Unmarshal(data, &rc); err != nil {
		return cc // malformed file → ignore (don't crash startup)
	}

	// Compile intimate trigger regex. We OR all configured patterns into one
	// regex so detection is a single match call. Patterns that fail to
	// compile are silently dropped (logged to stderr, never crash).
	var alts []string
	for _, p := range rc.IntimateTriggers {
		if _, err := regexp.Compile(p); err != nil {
			continue // skip invalid pattern
		}
		alts = append(alts, "(?:"+p+")")
	}
	if len(alts) > 0 {
		if re, err := regexp.Compile("(?i)(" + strings.Join(alts, "|") + ")"); err == nil {
			cc.intimate = re
		}
	}

	// Expand ~ in cwd prefixes
	h := os.Getenv("HOME")
	if h == "" {
		h = home
	}
	for _, p := range rc.TechnicalCwdPrefixes {
		if strings.HasPrefix(p, "~/") {
			p = filepath.Join(h, p[2:])
		} else if p == "~" {
			p = h
		}
		cc.cwdPrefixes = append(cc.cwdPrefixes, p)
	}

	for src, mode := range rc.SourceDefaults {
		if IsValidSoulMode(mode) {
			cc.sources[src] = mode
		}
	}

	if rc.Fallback != "" && IsValidSoulMode(rc.Fallback) {
		cc.fallback = SoulMode(rc.Fallback)
	}

	cc.fragmentIndexHint = strings.TrimSpace(rc.FragmentIndexHint)

	return cc
}

// detectSoulMode picks a SoulMode based on signals. Decision precedence:
//
//  1. Explicit override (env WEIRAN_SOUL_MODE / --soul-mode flag)
//  2. CLI mode hard mappings (these are framework-defined, not user data):
//     cron / heartbeat       → ops
//     evolve / evolve-probe  → evolve
//  3. First message matches user-configured intimate trigger regex → intimate
//     (triggers come from <workspace>/routing.yaml; absent = no triggers)
//  4. Launch directory has a user-configured technical prefix → technical
//  5. Source has a user-configured default (e.g. webui→emotional)
//  6. Fallback → routing.yaml `fallback:` field, default emotional
//
// All "soft" signals (triggers, prefixes, source defaults, fallback) come from
// the user's workspace routing.yaml, never hardcoded into framework code.
//
// Phase A: this function is called and audited but its result is NOT yet
// applied to actual fragment loading — we keep byte-equivalence with the
// pre-router prompt assembly until Phase C.
func detectSoulMode(sig RoutingSignals) SoulMode {
	if sig.Explicit != "" && IsValidSoulMode(sig.Explicit) {
		return SoulMode(sig.Explicit)
	}

	switch sig.CLIMode {
	case "cron", "heartbeat":
		return SoulModeOps
	case "evolve", "evolve-probe":
		return SoulModeEvolve
	}

	cfg := loadRoutingConfig()

	if sig.FirstMessage != "" && cfg.intimate != nil && cfg.intimate.MatchString(sig.FirstMessage) {
		return SoulModeIntimate
	}

	if sig.LaunchDir != "" {
		for _, prefix := range cfg.cwdPrefixes {
			if strings.HasPrefix(sig.LaunchDir, prefix) {
				return SoulModeTechnical
			}
		}
	}

	if mode, ok := cfg.sources[sig.Source]; ok {
		return SoulMode(mode)
	}

	return cfg.fallback
}

// detectSourceFromEnv infers RoutingSignals.Source from environment hints set
// by the spawning context (server, telegram bot, spawn child, cron job, ...).
// Falls back to "cli" when nothing is set.
func detectSourceFromEnv() string {
	for _, k := range []string{
		"WEIRAN_SESSION_SOURCE", // explicit override
		"SOUL_SESSION_SOURCE",   // alternate appName-derived (resolveSecrets style)
	} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	if os.Getenv("WEIRAN_SERVER_URL") != "" || os.Getenv("WEIRAN_AUTH_TOKEN") != "" {
		// We're a child of a weiran server (spawned session)
		return "spawn"
	}
	return "cli"
}

// gatherRoutingSignals collects all routing inputs from process state.
// Best-effort: missing fields are simply empty strings.
//
// When promptOverrides are active (server/spawn create paths set them via
// buildPromptWithOverrides), per-session LaunchDir / FirstMessage take
// precedence over the binary-wide launchDir / empty default. This is what
// makes intimate triggers and technical cwd prefixes actually fire in
// server mode — without it, every session sees the daemon's static cwd
// and an empty first message.
func gatherRoutingSignals() RoutingSignals {
	sig := RoutingSignals{
		CLIMode:      currentMode,
		LaunchDir:    launchDir,
		Source:       detectSourceFromEnv(),
		FirstMessage: "",
		Explicit:     os.Getenv("WEIRAN_SOUL_MODE"),
	}
	if activeOverrides != nil {
		if activeOverrides.LaunchDir != "" {
			sig.LaunchDir = activeOverrides.LaunchDir
		}
		if activeOverrides.FirstMessage != "" {
			sig.FirstMessage = activeOverrides.FirstMessage
		}
	}
	return sig
}

// PromptRoutingDecision is the audit record emitted after every buildPrompt.
type PromptRoutingDecision struct {
	Timestamp     time.Time
	CLIMode       string
	SoulMode      SoulMode
	Signals       RoutingSignals
	FragmentsUsed []string // Phase B: actual fragment paths; Phase A: empty (we still load monolithic SOUL.md)
	TokensEst     int
	CharSize      int
	SessionID     string // populated when known (server/spawn modes); empty for fresh CLI runs
}

// recordRoutingAudit writes a PromptRoutingDecision to the prompt_routing_audit
// table. Best-effort — failure to record never blocks prompt assembly.
func recordRoutingAudit(d PromptRoutingDecision) {
	db, err := openDB()
	if err != nil {
		return
	}
	defer db.Close()
	writeRoutingAudit(db, d)
}

// writeRoutingAudit performs the actual INSERT given an open DB handle.
// Separated for testability (tests pass an in-memory db).
func writeRoutingAudit(db *sql.DB, d PromptRoutingDecision) {
	signalsJSON, _ := json.Marshal(d.Signals)
	fragsJSON, _ := json.Marshal(d.FragmentsUsed)
	_, _ = db.Exec(`
		INSERT INTO prompt_routing_audit
		  (timestamp, cli_mode, soul_mode, signals_json, fragments_json,
		   tokens_est, char_size, session_id, launch_dir, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Timestamp.UTC().Format(time.RFC3339),
		d.CLIMode,
		string(d.SoulMode),
		string(signalsJSON),
		string(fragsJSON),
		d.TokensEst,
		d.CharSize,
		d.SessionID,
		d.Signals.LaunchDir,
		d.Signals.Source,
	)
}

// ── Phase B: Fragment Loader ──────────────────────────────────────────────────
//
// Soul fragments live under <workspace>/soul/**/*.md. Each fragment has YAML
// frontmatter declaring its id, title, and modes. The loader scans the directory,
// filters by mode, sorts by numeric filename prefix, and assembles content.
//
// Feature flag: enableFragmentLoading (env WEIRAN_FRAGMENT_LOADING=1, default false).
// When false, buildPrompt() is byte-identical to Phase A (legacy SOUL.md path).
// When true, the soul section is replaced by assembled fragments for the detected mode.

// enableFragmentLoading is the Phase-B feature flag.
// Default false = old SOUL.md path, byte-identical to pre-Phase-B output.
// Set WEIRAN_FRAGMENT_LOADING=1 to activate fragment-based assembly.
// var enableFragmentLoading = os.Getenv("WEIRAN_FRAGMENT_LOADING") == "1"
var enableFragmentLoading = true

// soulFragmentsDirOverride allows tests to inject a custom soul fragments directory.
// Empty string = use default (<workspace>/soul/).
var soulFragmentsDirOverride = ""

// getSoulFragmentsDir returns the soul fragments base directory.
func getSoulFragmentsDir() string {
	if soulFragmentsDirOverride != "" {
		return soulFragmentsDirOverride
	}
	return filepath.Join(workspace, "soul")
}

// parseModesList parses a YAML-inline list string like "[emotional, intimate, evolve]"
// into a slice of trimmed strings. Also handles bare comma-separated values.
func parseModesList(raw string) []string {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, "[]")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// fragmentModeIncludes reports whether mode is present in a raw modes string
// (e.g., "[emotional, intimate, evolve]").
func fragmentModeIncludes(modesStr string, mode SoulMode) bool {
	for _, m := range parseModesList(modesStr) {
		if m == string(mode) {
			return true
		}
	}
	return false
}

// extractFragmentSortKey extracts the leading integer from a filename for ordering.
// "01-identity.md" → 1, "50-appearance.md" → 50, "no-number.md" → 9999.
func extractFragmentSortKey(path string) int {
	base := filepath.Base(path)
	var numStr strings.Builder
	for _, c := range base {
		if c >= '0' && c <= '9' {
			numStr.WriteRune(c)
		} else {
			break
		}
	}
	if numStr.Len() == 0 {
		return 9999
	}
	n, _ := strconv.Atoi(numStr.String())
	return n
}

// loadFragmentsByMode returns the fragment file paths (in assembly order) for mode.
// It walks soulDir, reads frontmatter, filters by mode, and sorts by numeric prefix.
func loadFragmentsByMode(soulDir string, mode SoulMode) ([]string, error) {
	var matches []string
	err := filepath.Walk(soulDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		fm := parseMdFrontmatter(string(data))
		if fm == nil {
			return nil
		}
		if fragmentModeIncludes(fm["modes"], mode) {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(matches, func(i, j int) bool {
		ki := extractFragmentSortKey(matches[i])
		kj := extractFragmentSortKey(matches[j])
		if ki != kj {
			return ki < kj
		}
		return matches[i] < matches[j] // stable tie-break by path
	})
	return matches, nil
}

// stripFragmentFrontmatter removes the leading YAML frontmatter block (--- ... ---)
// from markdown content and returns the body, with leading blank lines trimmed.
func stripFragmentFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---") {
		return content
	}
	// Find the closing ---
	rest := content[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return content // malformed frontmatter — return as-is
	}
	body := rest[end+4:] // skip past \n---
	return strings.TrimLeft(body, "\n")
}

// assembleFragments reads fragment files, strips frontmatter, and concatenates them.
// Each fragment body is separated by a single blank line.
func assembleFragments(paths []string) (string, error) {
	var b strings.Builder
	for i, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("assembleFragments: read %s: %w", path, err)
		}
		body := stripFragmentFrontmatter(string(data))
		if i > 0 && b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(body)
		// Ensure fragment body ends with newline
		if !strings.HasSuffix(body, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String(), nil
}

// validateSoulFragments checks all soul/**/*.md files for proper frontmatter.
// Called by validateMdFormats() (hooks.go) during `weiran lint`.
// Returns a list of warning strings (empty = all valid).
func validateSoulFragments() []string {
	soulDir := getSoulFragmentsDir()
	if _, err := os.Stat(soulDir); os.IsNotExist(err) {
		// No soul dir yet — not an error in Phase B (fragments may not exist)
		return nil
	}

	var warnings []string
	_ = filepath.Walk(soulDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		label := "soul/" + strings.TrimPrefix(path, soulDir+"/")

		data, err := os.ReadFile(path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("📝 cannot read fragment: %s", label))
			return nil
		}
		content := string(data)

		fm := parseMdFrontmatter(content)
		if fm == nil {
			warnings = append(warnings, fmt.Sprintf("📝 missing frontmatter: %s", label))
			return nil
		}

		// Required fields: id, title, modes
		for _, field := range fragmentFrontmatterSpec.RequiredFields {
			if fm[field] == "" {
				warnings = append(warnings, fmt.Sprintf("📝 missing field '%s': %s", field, label))
			}
		}

		// id must match filename stem
		stem := strings.TrimSuffix(filepath.Base(path), ".md")
		if fm["id"] != "" && fm["id"] != stem {
			warnings = append(warnings, fmt.Sprintf("📝 id mismatch: file='%s' but id='%s' in %s", stem, fm["id"], label))
		}

		// modes: every value must be a valid SoulMode
		if modesStr := fm["modes"]; modesStr != "" {
			for _, m := range parseModesList(modesStr) {
				if !IsValidSoulMode(m) {
					warnings = append(warnings, fmt.Sprintf("📝 invalid mode '%s' in modes: %s", m, label))
				}
			}
			if len(parseModesList(modesStr)) == 0 {
				warnings = append(warnings, fmt.Sprintf("📝 empty modes list: %s", label))
			}
		}

		return nil
	})
	return warnings
}
