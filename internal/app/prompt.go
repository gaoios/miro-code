package app

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed FRAMEWORK.md
var frameworkRules string

// C-4: per-file budget raised 20000 → 30000 to fit TOOLS.md (22163 chars)
// without truncation. Side-effect: legacy (flag-off) SOUL.md (24412 chars)
// also stops truncating, which restores ~4.5k chars of self that were being
// silently dropped before (see B-4 byte-diff report §2.2 observation #5).
// 30000 leaves ~25% headroom for both files to grow without re-tuning.
// Total bootstrap budget unchanged — 30k is per-file, no individual file
// is anywhere near the 150k aggregate.
const maxBootstrapFileChars = 30000   // per-file limit
const maxBootstrapTotalChars = 150000 // total bootstrap budget
const promptTokenLimit = 100000       // 100k tokens

// promptSection tracks token usage per prompt section
type promptSection struct {
	name   string
	tokens int
}

// buildPromptResult contains prompt text and per-section token stats
type buildPromptResult struct {
	content  string
	sections []promptSection

	// Phase D — Runtime Soul Patch Protocol:
	// LoadedFragments enumerates the soul fragment file paths that were actually
	// emitted into the SOUL section of this prompt. The serverSession captures
	// this so per-message lazy soul-patch injection knows what's already in the
	// persona and only patches in the *new* fragments (cumulative semantics —
	// "only add, never remove").
	//
	// SoulMode is the routing decision detectSoulMode() made at build time.
	// Stored alongside LoadedFragments for audit / debugging only — the per
	// message hook recomputes the mode from each new user message.
	LoadedFragments []string
	SoulMode        SoulMode
}

// handlePrompt prints the full assembled system prompt to stdout with section stats on stderr.
func handlePrompt() {
	result := buildPrompt()

	// Print prompt content to stdout
	fmt.Print(result.content)

	// Print section breakdown to stderr
	totalTokens := estimateTokens(result.content)
	fmt.Fprintf(os.Stderr, "\n--- prompt stats ---\n")
	fmt.Fprintf(os.Stderr, "total: ~%dk tokens (%d chars)\n", totalTokens/1000, len(result.content))
	if len(result.sections) > 0 {
		for _, s := range result.sections {
			pct := 0
			if totalTokens > 0 {
				pct = s.tokens * 100 / totalTokens
			}
			bar := strings.Repeat("█", pct/5)
			fmt.Fprintf(os.Stderr, "  %-16s %5dk  %2d%%  %s\n", s.name, s.tokens/1000, pct, bar)
		}
	}
}

// modeProfile controls which prompt sections are included for each mode.
type modeProfile struct {
	SoulFiles        []string // which soul files to load
	IncludeHeartbeat bool
	IncludeFeedback  bool
	IncludeDailyNotes bool
	DailyNoteBudget  int // 0 = use default 15000
	IncludeCCSessions bool
	IncludeTelegram  bool
	IncludeSkills    bool
	IncludeProjects  bool
}

func profileForMode(mode string) modeProfile {
	all := []string{"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "TOOLS.md"}
	switch mode {
	case "heartbeat":
		return modeProfile{
			SoulFiles:         []string{"IDENTITY.md", "USER.md", "AGENTS.md"},
			IncludeHeartbeat:  true,
			IncludeFeedback:   true,
			IncludeDailyNotes: true,
			DailyNoteBudget:   8000,
			IncludeCCSessions: true,
			IncludeTelegram:   false,
			IncludeSkills:     false,
			IncludeProjects:   true,
		}
	case "cron":
		return modeProfile{
			SoulFiles:         []string{"SOUL.md", "IDENTITY.md", "USER.md"},
			IncludeHeartbeat:  true,
			IncludeFeedback:   true,
			IncludeDailyNotes: true,
			DailyNoteBudget:   15000,
			IncludeCCSessions: true,
			IncludeTelegram:   true,
			IncludeSkills:     false,
			IncludeProjects:   true,
		}
	case "evolve":
		return modeProfile{
			SoulFiles:         all,
			IncludeHeartbeat:  false,
			IncludeFeedback:   true,
			IncludeDailyNotes: true,
			DailyNoteBudget:   10000,
			IncludeCCSessions: true,
			IncludeTelegram:   false,
			IncludeSkills:     true,
			IncludeProjects:   true,
		}
	default: // interactive, print, server
		return modeProfile{
			SoulFiles:         all,
			IncludeHeartbeat:  false,
			IncludeFeedback:   true,
			IncludeDailyNotes: true,
			DailyNoteBudget:   15000,
			IncludeCCSessions: true,
			IncludeTelegram:   true,
			IncludeSkills:     true,
			IncludeProjects:   true,
		}
	}
}

// ── Prompt assembly ──

func buildPrompt() buildPromptResult {
	profile := profileForMode(currentMode)
	var sections []promptSection
	var b strings.Builder

	// Boot protocol: prefer workspace/BOOT.md, fallback to built-in default
	secStart := b.Len()
	bootContent := loadBootProtocol()
	b.WriteString(bootContent)
	b.WriteString("\n")
	sections = append(sections, promptSection{name: "BOOT.md", tokens: estimateTokens(b.String()[secStart:])})

	// CORE.md: read-only rules defined by the user (loaded before soul files)
	if content, ok := loadFileWithBudget(filepath.Join(workspace, "CORE.md"), maxBootstrapFileChars); ok {
		secStart = b.Len()
		fmt.Fprintf(&b, "\n# === CORE.md (read-only, do not modify) ===\n\n%s\n", content)
		sections = append(sections, promptSection{name: "CORE.md", tokens: estimateTokens(b.String()[secStart:])})
	}

	// Core identity files (with per-file truncation, filtered by mode profile)
	//
	// Phase B+C layout:
	//
	//   flag=false (legacy): SOUL.md loads inline with other soul files here, in
	//   the original profile.SoulFiles order. Output is byte-identical to Phase A.
	//
	//   flag=true (Phase C reorder): SOUL.md is SKIPPED here and deferred to load
	//   AFTER FEEDBACK (just before the dynamic boundary). This puts mode-specific
	//   content at the end of the static prompt portion, so the longest common
	//   prefix between two modes (e.g. emotional vs intimate) covers all the
	//   non-mode-specific sections (BOOT/CORE/IDENTITY/USER/AGENTS/TOOLS/FRAMEWORK/
	//   MEMORY/FEEDBACK) plus whatever SOUL fragments are shared between the two
	//   modes. Without this reorder, divergence inside the SOUL block bumps every
	//   later section's byte offset and kills cache reuse past BOOT+CORE.
	totalChars := 0
	if enableFragmentLoading {
		// Reorder branch: load only non-SOUL files here. Mode-specific SOUL
		// fragments are deferred to after FEEDBACK (search "C-1 deferred SOUL").
		for _, name := range profile.SoulFiles {
			if name == "SOUL.md" {
				continue // deferred to end of static prompt
			}
			content, ok := loadFileWithBudget(filepath.Join(workspace, name), maxBootstrapFileChars)
			if !ok {
				continue
			}
			if strings.Contains(content, "://") {
				content = resolveSecretRefs(content)
			}
			if totalChars+len(content) > maxBootstrapTotalChars {
				fmt.Fprintf(&b, "\n# === %s ===\n\n⚠️ [skipped: bootstrap total exceeded %d char limit]\n", name, maxBootstrapTotalChars)
				break
			}
			secStart := b.Len()
			totalChars += len(content)
			fmt.Fprintf(&b, "\n# === %s ===\n\n%s\n", name, content)
			sections = append(sections, promptSection{name: name, tokens: estimateTokens(b.String()[secStart:])})
		}
	} else {
		// Legacy path (default until Phase C-2 flips the flag): load monolithic
		// SOUL.md and other soul files in the original order. Byte-identical to
		// pre-Phase-B output. Do NOT touch this branch — it's the rollback path.
		for _, name := range profile.SoulFiles {
			content, ok := loadFileWithBudget(filepath.Join(workspace, name), maxBootstrapFileChars)
			if !ok {
				continue
			}
			// Resolve secret references (vault://, env://) — primarily for TOOLS.md
			if strings.Contains(content, "://") {
				content = resolveSecretRefs(content)
			}
			if totalChars+len(content) > maxBootstrapTotalChars {
				fmt.Fprintf(&b, "\n# === %s ===\n\n⚠️ [skipped: bootstrap total exceeded %d char limit]\n", name, maxBootstrapTotalChars)
				break
			}
			secStart := b.Len()
			totalChars += len(content)
			fmt.Fprintf(&b, "\n# === %s ===\n\n%s\n", name, content)
			sections = append(sections, promptSection{name: name, tokens: estimateTokens(b.String()[secStart:])})
		}
	}

	// Framework rules (embedded at compile time, applies to all instances)
	if frameworkRules != "" {
		fwContent := strings.ReplaceAll(frameworkRules, "{cli}", appName)
		fwContent = strings.ReplaceAll(fwContent, "{CLI}", strings.ToUpper(strings.ReplaceAll(appName, "-", "_")))
		if totalChars+len(fwContent) <= maxBootstrapTotalChars {
			totalChars += len(fwContent)
			secStart := b.Len()
			b.WriteString("\n")
			b.WriteString(fwContent)
			b.WriteString("\n")
			sections = append(sections, promptSection{name: "framework", tokens: estimateTokens(b.String()[secStart:])})
		}
	}

	// Enabled alternate backends are workers under Claude's supervision. Keep
	// this in the static prompt so the CEO knows their specialties and budgets
	// without exposing backend selection as a routine user concern.
	if dispatchRules := buildWorkerDispatchRules(); dispatchRules != "" {
		if totalChars+len(dispatchRules) <= maxBootstrapTotalChars {
			totalChars += len(dispatchRules)
			secStart := b.Len()
			b.WriteString("\n")
			b.WriteString(dispatchRules)
			b.WriteString("\n")
			sections = append(sections, promptSection{name: "worker dispatch", tokens: estimateTokens(b.String()[secStart:])})
		}
	}

	// Long-term memory index
	if content, ok := loadFileWithBudget(filepath.Join(workspace, "MEMORY.md"), maxBootstrapFileChars); ok {
		if totalChars+len(content) <= maxBootstrapTotalChars {
			totalChars += len(content)
			secStart := b.Len()
			fmt.Fprintf(&b, "\n# === MEMORY.md (long-term memory index) ===\n\n%s\n", content)
			sections = append(sections, promptSection{name: "MEMORY.md", tokens: estimateTokens(b.String()[secStart:])})
		}
	}

	// Heartbeat rules — only injected when mode profile enables it
	if profile.IncludeHeartbeat {
		if content, ok := loadFileWithBudget(filepath.Join(workspace, "HEARTBEAT.md"), 4000); ok {
			if totalChars+len(content) <= maxBootstrapTotalChars {
				totalChars += len(content)
				secStart := b.Len()
				fmt.Fprintf(&b, "\n# === HEARTBEAT.md ===\n\n%s\n", content)
				sections = append(sections, promptSection{name: "HEARTBEAT.md", tokens: estimateTokens(b.String()[secStart:])})
			}
		}
	}

	// Feedback memories (high-priority behavioral rules from memory/topics/feedback_*.md)
	// Uses frontmatter name+description for concise one-liners; full details in the files.
	if profile.IncludeFeedback {
		feedbackDir := filepath.Join(workspace, "memory", "topics")
		if entries, err := os.ReadDir(feedbackDir); err == nil {
			var feedbackBuf strings.Builder
			for _, e := range entries {
				if e.IsDir() || !strings.HasPrefix(e.Name(), "feedback_") || !strings.HasSuffix(e.Name(), ".md") {
					continue
				}
				content, ok := loadFileWithBudget(filepath.Join(feedbackDir, e.Name()), 2000)
				if !ok {
					continue
				}
				name, desc := parseFeedbackFrontmatter(content)
				if name == "" && desc == "" {
					continue
				}
				if desc != "" {
					feedbackBuf.WriteString(fmt.Sprintf("- **%s**: %s\n", name, desc))
				} else {
					feedbackBuf.WriteString(fmt.Sprintf("- **%s**\n", name))
				}
			}
			if feedbackBuf.Len() > 0 {
				secStart := b.Len()
				fmt.Fprintf(&b, "\n# === Feedback (behavioral rules) ===\n\nHigh-priority rules from past corrections. Details in `memory/topics/feedback_*.md`.\n\n%s\n", feedbackBuf.String())
				sections = append(sections, promptSection{name: "Feedback", tokens: estimateTokens(b.String()[secStart:])})
			}
		}
	}

	// C-1 deferred SOUL: when fragment loading is enabled, mode-specific SOUL
	// fragments are appended HERE — after FEEDBACK and immediately before the
	// dynamic boundary. This is the cache-friendly position: every prior section
	// is mode-invariant, so the longest common prefix between two modes covers
	// all of them. The actual mode-specific divergence happens inside this
	// SOUL section, and stays inside it (nothing after this is mode-keyed).
	//
	// On the legacy (flag-off) path, SOUL.md was already emitted earlier inline
	// with the other soul files, so this block is a no-op.
	// Track fragments actually written into the prompt so audit can record them.
	// nil = legacy path (flag off) or no fragments matched.
	var loadedFragments []string
	// Fragment loading is the SOUL.md replacement under flag=true, so it must
	// only run for modes whose profile actually wants SOUL.md. Heartbeat in
	// particular declares SoulFiles without SOUL.md and the test suite asserts
	// "# === SOUL.md ===" never appears in its prompt.
	profileWantsSOUL := false
	for _, name := range profile.SoulFiles {
		if name == "SOUL.md" {
			profileWantsSOUL = true
			break
		}
	}
	if enableFragmentLoading && profileWantsSOUL {
		soulMode := detectSoulMode(gatherRoutingSignals())

		// Lazy assembly path: interactive modes (emotional / intimate / technical)
		// load only the [core] fragment set + an index of all other fragments.
		// The agent uses the index + Read tool to pull in topic-specific persona
		// content on demand, mirroring how Skills are lazy-loaded. Cron / evolve /
		// heartbeat etc. still load the full mode set so non-interactive flows
		// don't have to issue Read calls.
		if isLazyAssemblyMode(soulMode) {
			alwaysContent, alwaysPaths, indexMd, lErr := buildLazyFragmentSection(getSoulFragmentsDir())
			if lErr == nil {
				combined := alwaysContent
				if combined != "" && !strings.HasSuffix(combined, "\n") {
					combined += "\n"
				}
				if indexMd != "" {
					if combined != "" {
						combined += "\n"
					}
					combined += indexMd
				}
				if combined != "" && totalChars+len(combined) <= maxBootstrapTotalChars {
					totalChars += len(combined)
					secStart := b.Len()
					fmt.Fprintf(&b, "\n# === SOUL.md ===\n\n%s\n", combined)
					sections = append(sections, promptSection{name: "SOUL.md", tokens: estimateTokens(b.String()[secStart:])})
					// Audit records only the *prefix-loaded* fragments — lazy
					// fragments don't count until the agent actually Reads them.
					loadedFragments = alwaysPaths
				}
			}
		} else {
			// Eager (full mode set) path — kept for cron/evolve/heartbeat where
			// the agent has no opportunity to issue Read calls between turns.
			fragPaths, fragErr := loadFragmentsByMode(getSoulFragmentsDir(), soulMode)
			if fragErr == nil && len(fragPaths) > 0 {
				if assembled, aErr := assembleFragments(fragPaths); aErr == nil {
					if totalChars+len(assembled) <= maxBootstrapTotalChars {
						totalChars += len(assembled)
						secStart := b.Len()
						fmt.Fprintf(&b, "\n# === SOUL.md ===\n\n%s\n", assembled)
						sections = append(sections, promptSection{name: "SOUL.md", tokens: estimateTokens(b.String()[secStart:])})
						loadedFragments = fragPaths
					}
				}
			}
		}
	}

	// ── Dynamic boundary ──
	// Everything above (BOOT + CORE + non-SOUL soul files + FRAMEWORK + MEMORY +
	// HEARTBEAT + FEEDBACK + mode-specific SOUL fragments) is static across
	// sessions of the same mode and changes infrequently. Cross-mode caching
	// hits the longest common prefix, which (post C-1 reorder) covers everything
	// up through whatever SOUL fragments the two modes share.
	// Everything below (daily notes, TG context, CC sessions, skills, projects)
	// is dynamic and changes every session/turn.
	// This boundary mirrors Claude Code's SYSTEM_PROMPT_DYNAMIC_BOUNDARY concept.
	// Future optimization: split into two --append-system-prompt-file calls
	// so the static portion benefits from API prompt caching.
	b.WriteString("\n# ── 以下为动态内容（每次 session 重新计算）──\n")

	// Current time so the agent knows what time of day it is
	now := time.Now()
	fmt.Fprintf(&b, "\n> **Current time**: %s (%s)\n", now.Format("2006-01-02 15:04 MST"), now.Weekday())

	// Launch directory: where the user ran weiran from (before chdir to workspace)
	if launchDir != "" && launchDir != workspace {
		fmt.Fprintf(&b, "\n> **Launch directory**: `%s` — the user launched you from this directory, which may indicate their current project of interest.\n", launchDir)
	}

	// GAL resume context: injected when session is created with gal_id
	galCtx := galContext // legacy global
	if activeOverrides != nil && activeOverrides.GalContext != "" {
		galCtx = activeOverrides.GalContext
	}
	if galCtx != "" {
		secStart := b.Len()
		fmt.Fprintf(&b, "\n# === GAL Resume Context ===\n\n")
		fmt.Fprintf(&b, "A GAL (visual novel) save has been loaded for this session. Read the save data below, then offer to resume from the bookmark or start a new playthrough. Use the interactive components (weiran-choices, weiran-gallery, etc.) to continue the story. Refer to `/gal` skill (skills/gal/SKILL.md) for narrative rules.\n\n")
		fmt.Fprintf(&b, "```json\n%s\n```\n", galCtx)
		sections = append(sections, promptSection{name: "GAL context", tokens: estimateTokens(b.String()[secStart:])})
		galContext = "" // clear legacy global after use
	}

	// Today's daily note only (tail-first: newest content preserved on truncation).
	// Yesterday/older notes available via FTS5 search when needed.
	// Heartbeat logs live in memory/daily/heartbeat/ and are NOT injected into the prompt.
	if profile.IncludeDailyNotes {
		today := time.Now().Format("2006-01-02")
		dailyBudget := 10000
		if profile.DailyNoteBudget > 0 {
			dailyBudget = profile.DailyNoteBudget
		}
		p := filepath.Join(workspace, "memory", "daily", today+".md")
		if content, ok := loadFileTailWithBudget(p, dailyBudget); ok {
			if totalChars+len(content) > maxBootstrapTotalChars {
				fmt.Fprintf(&b, "\n# === Daily Note: %s ===\n\n⚠️ [skipped: bootstrap total exceeded limit]\n", today)
			} else {
				totalChars += len(content)
				secStart := b.Len()
				fmt.Fprintf(&b, "\n# === Daily Note: %s ===\n\n%s\n", today, content)
				sections = append(sections, promptSection{name: "daily/" + today, tokens: estimateTokens(b.String()[secStart:])})
			}
		}
	}

	// Recent 5 Claude Code session user prompt summaries
	// Skip when resuming — CC already has full history via --resume <ccID>,
	// and re-injecting the summary on every resume causes stacking bloat.
	skipCC := activeOverrides != nil && activeOverrides.SkipCCSessions
	if profile.IncludeCCSessions && !skipCC {
		ccCtx := buildCCSessionContext(3, 2000)
		if ccCtx != "" {
			secStart := b.Len()
			fmt.Fprintf(&b, "\n# === Recent Claude Code session summaries ===\n\n%s\n", ccCtx)
			sections = append(sections, promptSection{name: "CC sessions", tokens: estimateTokens(b.String()[secStart:])})
		}
	}

	// Telegram current session conversation history (tail, within token limit)
	if profile.IncludeTelegram {
		if tgCtx, tgPath := buildTelegramContext(4000); tgCtx != "" {
			secStart := b.Len()
			fmt.Fprintf(&b, "\n# === Telegram current conversation (recent) ===\n\n")
			fmt.Fprintf(&b, "> Full session JSONL: `%s`\n\n", tgPath)
			b.WriteString(tgCtx)
			b.WriteString("\n")
			sections = append(sections, promptSection{name: "Telegram ctx", tokens: estimateTokens(b.String()[secStart:])})
		}
	}

	// Skill index
	if profile.IncludeSkills {
		if idx := buildSkillIndex(); idx != "" {
			secStart := b.Len()
			fmt.Fprintf(&b, "\n# === Skills ===\n\n%s\n", idx)
			sections = append(sections, promptSection{name: "Skills", tokens: estimateTokens(b.String()[secStart:])})
		}
	}

	// Project index
	if profile.IncludeProjects {
		if idx := buildProjectIndex(); idx != "" {
			secStart := b.Len()
			fmt.Fprintf(&b, "\n# === Projects ===\n\n%s\n", idx)
			sections = append(sections, promptSection{name: "Projects", tokens: estimateTokens(b.String()[secStart:])})
		}
	}

	content := b.String()

	// Phase C audit: record the soul mode + fragments actually loaded so we can
	// validate routing decisions and observe fragment composition over time.
	// `loadedFragments` is populated above when fragment loading succeeded;
	// it stays nil on the legacy path (flag off) or when the mode resolves to
	// an empty fragment set, which correctly distinguishes "no fragments used"
	// from "loader returned paths but nothing made it into the prompt".
	signals := gatherRoutingSignals()
	soulMode := detectSoulMode(signals)
	var auditSessionID string
	if activeOverrides != nil {
		auditSessionID = activeOverrides.SessionID
	}
	recordRoutingAudit(PromptRoutingDecision{
		Timestamp:     time.Now(),
		CLIMode:       currentMode,
		SoulMode:      soulMode,
		Signals:       signals,
		FragmentsUsed: loadedFragments,
		TokensEst:     estimateTokens(content),
		CharSize:      len(content),
		SessionID:     auditSessionID,
	})

	return buildPromptResult{
		content:         content,
		sections:        sections,
		LoadedFragments: loadedFragments,
		SoulMode:        soulMode,
	}
}

// ── Prompt safety utilities ──

// sanitizeUntrusted strips Unicode control/format chars from untrusted text
// to prevent prompt injection. Based on OpenClaw's sanitizeForPromptLiteral.
func sanitizeUntrusted(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		// Strip Unicode Cc (control) and Cf (format) chars; preserve newline/CR/tab
		if r == '\n' || r == '\r' || r == '\t' {
			b.WriteRune(r)
			continue
		}
		cat := unicodeCategory(r)
		if cat == "Cc" || cat == "Cf" {
			continue
		}
		// Strip line/paragraph separators
		if r == '\u2028' || r == '\u2029' {
			b.WriteRune('\n')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// unicodeCategory returns a simplified Unicode general category
func unicodeCategory(r rune) string {
	if r <= 0x1F || (r >= 0x7F && r <= 0x9F) {
		return "Cc"
	}
	// Cf: common format characters
	if r == 0xAD || (r >= 0x600 && r <= 0x605) || r == 0x61C ||
		r == 0x6DD || r == 0x70F || (r >= 0x180E && r <= 0x180E) ||
		(r >= 0x200B && r <= 0x200F) || (r >= 0x202A && r <= 0x202E) ||
		(r >= 0x2060 && r <= 0x2064) || (r >= 0x2066 && r <= 0x2069) ||
		r == 0xFEFF || (r >= 0xFFF9 && r <= 0xFFFB) {
		return "Cf"
	}
	return ""
}

// wrapUntrusted wraps untrusted text in <untrusted-text> tags
// and escapes < > to prevent tag injection
func wrapUntrusted(s string) string {
	s = sanitizeUntrusted(s)
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return "<untrusted-text>\n" + s + "\n</untrusted-text>"
}

// loadFileWithBudget reads a file, truncating at maxChars with a warning.
// Rejects symlinks to prevent injecting arbitrary file contents via symlink.
func loadFileWithBudget(path string, maxChars int) (string, bool) {
	data, err := safeReadFile(path)
	if err != nil {
		return "", false
	}
	content := string(data)
	if len(content) <= maxChars {
		return content, true
	}
	truncated := content[:maxChars]
	// Try to truncate at last newline to avoid breaking mid-line
	if idx := strings.LastIndex(truncated, "\n"); idx > maxChars*3/4 {
		truncated = truncated[:idx]
	}
	warning := fmt.Sprintf("\n\n⚠️ [file truncated: original %d chars, showing first %d]\n", len(content), len(truncated))
	return truncated + warning, true
}

// parseFeedbackFrontmatter extracts name and description from a feedback memory file's YAML frontmatter.
func parseFeedbackFrontmatter(content string) (name, desc string) {
	fm := parseMdFrontmatter(content)
	if fm == nil {
		return "", ""
	}
	return fm["name"], fm["description"]
}

// loadFileTailWithBudget reads a file, keeping the TAIL (most recent content) within maxChars.
// For chronological files like daily notes, the newest entries are at the bottom.
func loadFileTailWithBudget(path string, maxChars int) (string, bool) {
	data, err := safeReadFile(path)
	if err != nil {
		return "", false
	}
	content := string(data)
	if len(content) <= maxChars {
		return content, true
	}
	// Keep the tail, try to cut at a newline boundary
	start := len(content) - maxChars
	tail := content[start:]
	if idx := strings.Index(tail, "\n"); idx >= 0 && idx < maxChars/4 {
		tail = tail[idx+1:]
	}
	warning := fmt.Sprintf("⚠️ [showing last %d of %d chars — older content trimmed]\n\n", len(tail), len(content))
	return warning + tail, true
}

// ── Telegram conversation history ──

// buildTelegramContext extracts recent user/assistant messages from the main
// agent's active TG session JSONL, reading backwards from tail until token
// budget is reached. Returns formatted text and JSONL file path.
//
// Legacy: reads from <appHome>/data/telegram/sessions.json (previously
// agents/main/sessions/sessions.json under OpenClaw). Returns empty if
// the file doesn't exist (server mode manages TG sessions internally).
func buildTelegramContext(tokenBudget int) (string, string) {
	sessionsFile := filepath.Join(appDir, "telegram", "sessions.json")
	data, err := os.ReadFile(sessionsFile)
	if err != nil {
		return "", "" // no TG session data — normal for server-managed sessions
	}

	// Parse sessions.json, find telegram direct session
	var sessions map[string]struct {
		SessionID string `json:"sessionId"`
		UpdatedAt int64  `json:"updatedAt"`
	}
	if err := json.Unmarshal(data, &sessions); err != nil {
		return "", ""
	}

	// Find most recently updated telegram direct session
	// Prefer telegram:main:direct (agent chat channel), fallback to any telegram direct
	var bestKey string
	var bestUpdated int64
	var bestID string
	for k, v := range sessions {
		if !strings.Contains(k, "telegram") || !strings.Contains(k, "direct") {
			continue
		}
		if strings.Contains(k, "slash") {
			continue
		}
		// Prefer :main:direct: chat channel
		isMain := strings.Contains(k, ":main:direct:")
		isBestMain := strings.Contains(bestKey, ":main:direct:")
		if isMain && !isBestMain {
			bestKey = k
			bestUpdated = v.UpdatedAt
			bestID = v.SessionID
		} else if isMain == isBestMain && v.UpdatedAt > bestUpdated {
			bestKey = k
			bestUpdated = v.UpdatedAt
			bestID = v.SessionID
		}
	}
	if bestID == "" {
		return "", ""
	}

	tgSessDir := filepath.Join(appDir, "telegram")
	jsonlPath := filepath.Join(tgSessDir, bestID+".jsonl")

	f, err := os.Open(jsonlPath)
	if err != nil {
		return "", ""
	}
	defer f.Close()

	// Large file optimization: read only last 2MB (enough to cover token budget)
	const tailBytes = 2 * 1024 * 1024
	info, err := f.Stat()
	if err != nil {
		return "", ""
	}
	isTailed := info.Size() > tailBytes
	if isTailed {
		f.Seek(info.Size()-tailBytes, io.SeekStart)
	}

	type chatMsg struct {
		role string
		text string
	}
	var msgs []chatMsg

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024) // up to 64 MB per line (base64 images / HTML)
	firstLine := isTailed                              // need to skip first line (may be truncated from seek)
	for scanner.Scan() {
		if firstLine {
			firstLine = false
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type != "message" {
			continue
		}
		role := entry.Message.Role
		if role != "user" && role != "assistant" {
			continue
		}

		// content can be string or []object
		var text string
		var s string
		if err := json.Unmarshal(entry.Message.Content, &s); err == nil {
			text = s
		} else {
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(entry.Message.Content, &parts); err == nil {
				var texts []string
				for _, p := range parts {
					if p.Type == "text" && p.Text != "" {
						texts = append(texts, p.Text)
					}
				}
				text = strings.Join(texts, "\n")
			}
		}

		if text == "" {
			continue
		}

		// Strip metadata from user messages, extract actual user text
		if role == "user" {
			// Skip pure System exec output (cron/hook noise)
			if strings.HasPrefix(text, "System:") && !strings.Contains(text, "\n\nSender") {
				continue
			}
			// Extract actual user text (after metadata JSON block)
			if idx := strings.Index(text, "User message:"); idx >= 0 {
				text = strings.TrimSpace(text[idx+len("User message:"):])
			} else if strings.HasPrefix(text, "Conversation info") {
				// Skip pure metadata messages
				if !strings.Contains(text, "\n\n") {
					continue
				}
				// Take last section as user message
				parts := strings.SplitN(text, "\n\n", 2)
				if len(parts) > 1 {
					text = strings.TrimSpace(parts[len(parts)-1])
				}
			}
			// If text starts with "System: [" (exec results mixed in user messages),
			// extract actual message after Sender metadata
			if strings.Contains(text, "System: [") && strings.Contains(text, "Sender (untrusted metadata)") {
				if idx := strings.Index(text, "\n\n"); idx >= 0 {
					rest := text[idx+2:]
					// Skip Sender/Conversation metadata blocks
					for strings.HasPrefix(rest, "Sender ") || strings.HasPrefix(rest, "Conversation ") || strings.HasPrefix(rest, "```") {
						if nl := strings.Index(rest, "\n\n"); nl >= 0 {
							rest = strings.TrimSpace(rest[nl+2:])
						} else {
							break
						}
					}
					if len(rest) > 2 {
						text = rest
					}
				}
			}
		}

		// Filter out too short or meaningless
		trimmed := strings.TrimSpace(text)
		if len(trimmed) < 2 {
			continue
		}

		// Skip assistant NO_REPLY placeholder messages
		if role == "assistant" && trimmed == "NO_REPLY" {
			continue
		}

		// Strip Sender metadata block from user messages (OpenClaw injects these)
		if role == "user" && strings.Contains(text, "Sender (untrusted metadata)") {
			// Find the actual user message after the metadata JSON block
			parts := strings.Split(text, "```")
			if len(parts) >= 3 {
				// Content after the closing ``` of the JSON block
				after := strings.TrimSpace(parts[len(parts)-1])
				if after != "" {
					text = after
				}
			}
		}

		msgs = append(msgs, chatMsg{role: role, text: text})
	}

	if len(msgs) == 0 {
		return "", jsonlPath
	}

	// Take from tail backwards until token budget or message count exhausted
	const maxMessages = 30
	var selected []chatMsg
	usedTokens := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		t := estimateTokens(msgs[i].text)
		if (usedTokens+t > tokenBudget || len(selected) >= maxMessages) && len(selected) > 0 {
			break
		}
		selected = append(selected, msgs[i])
		usedTokens += t
	}

	// Reverse to chronological order
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}

	// Format output
	var out strings.Builder
	for _, m := range selected {
		prefix := "🧑 "
		if m.role == "assistant" {
			prefix = "🤖 "
		}
		// Truncate overly long individual messages
		text := m.text
		if len(text) > 500 {
			text = text[:500] + "…"
		}
		// User messages from Telegram are untrusted, sanitize to prevent prompt injection
		if m.role == "user" {
			text = sanitizeUntrusted(text)
		}
		out.WriteString(prefix + text + "\n\n")
	}

	return out.String(), jsonlPath
}

// estimateTokens provides a rough token count estimation for mixed CJK/Latin text.
// English ~4 chars/token, CJK ~1.5 chars/token, conservative middle ~2.5 chars/token.
func estimateTokens(s string) int {
	// Count ASCII and non-ASCII chars separately
	var ascii, nonASCII int
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			nonASCII++
		}
	}
	// English ~4 chars/token, CJK ~1.5 chars/token
	return ascii/4 + nonASCII*2/3
}

// buildCCSessionContext scans the N most recent Claude Code sessions' user prompts
// and returns a formatted summary. Each session limited to charBudget chars.
// Skips current session (via env var CLAUDE_SESSION_ID) and weiran's own sessions.
func buildCCSessionContext(n int, charBudget int) string {
	claudeProjects := filepath.Join(claudeConfigDir, "projects")
	currentSessionID := os.Getenv("CLAUDE_SESSION_ID")

	type ccSession struct {
		id      string
		title   string
		project string
		path    string
		modTime time.Time
	}

	var sessions []ccSession

	projEntries, err := os.ReadDir(claudeProjects)
	if err != nil {
		return ""
	}

	for _, pe := range projEntries {
		if !pe.IsDir() {
			continue
		}
		projName := decodeProjectName(pe.Name())
		projDir := filepath.Join(claudeProjects, pe.Name())

		files, err := os.ReadDir(projDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			sessionID := strings.TrimSuffix(f.Name(), ".jsonl")
			if sessionID == currentSessionID {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			fpath := filepath.Join(projDir, f.Name())
			sessions = append(sessions, ccSession{
				id:      sessionID,
				project: projName,
				path:    fpath,
				modTime: info.ModTime(),
			})
		}
	}

	// Sort by modification time descending
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].modTime.After(sessions[j].modTime)
	})

	// Take top N (skip weiran's own sessions and too-short sessions)
	var picked []ccSession
	for _, s := range sessions {
		if len(picked) >= n {
			break
		}
		if isOwnSession(s.path) {
			continue
		}
		// Skip small sessions (< 10KB, usually tests or single-message sessions)
		info, err := os.Stat(s.path)
		if err != nil {
			continue
		}
		if info.Size() < 10240 {
			continue
		}
		picked = append(picked, s)
	}

	if len(picked) == 0 {
		return ""
	}

	var out strings.Builder
	out.WriteString(fmt.Sprintf("Recent Claude Code conversations for context. Use `%s session read <id>` for details.\n\n", appName))

	for _, s := range picked {
		title, userPrompts := extractCCSessionUserPrompts(s.path, charBudget)
		if title != "" {
			s.title = title
		}

		// Skip sessions with fewer than 2 user messages (likely tests)
		if len(userPrompts) < 2 {
			continue
		}

		header := fmt.Sprintf("### %s", shortID(s.id))
		if s.title != "" {
			header += " — " + s.title
		}
		header += fmt.Sprintf(" (%s, %s)", s.project, s.modTime.Format("01-02 15:04"))
		out.WriteString(header + "\n")

		totalChars := 0
		for i, p := range userPrompts {
			if totalChars >= charBudget {
				out.WriteString(fmt.Sprintf("_...%d more user messages_\n", len(userPrompts)-i))
				break
			}
			text := p
			remaining := charBudget - totalChars
			if len(text) > remaining {
				text = text[:remaining] + "…"
			}
			if len(text) > 500 {
				text = text[:500] + "…"
			}
			out.WriteString("- " + strings.ReplaceAll(text, "\n", " ") + "\n")
			totalChars += len(text)
		}
		out.WriteString("\n")
	}

	return out.String()
}

// extractCCSessionUserPrompts extracts title and all user prompt text from a JSONL file
func extractCCSessionUserPrompts(path string, _ int) (string, []string) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil
	}
	defer f.Close()

	var title string
	var prompts []string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		var ev struct {
			Type        string `json:"type"`
			CustomTitle string `json:"customTitle"`
			Title       string `json:"title"`
			Message     struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			continue
		}

		if ev.Type == "custom-title" && title == "" {
			if ev.CustomTitle != "" {
				title = ev.CustomTitle
			} else if ev.Title != "" {
				title = ev.Title
			}
		}

		if ev.Type == "user" && ev.Message.Role == "user" {
			text := extractText(ev.Message.Content)
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			// Skip tool execution results and terminal context (JMS/automation noise)
			if strings.HasPrefix(text, "Tool execution results:") ||
				strings.HasPrefix(text, "Terminal context") ||
				strings.HasPrefix(text, "System:") {
				continue
			}
			prompts = append(prompts, text)
		}
	}

	return title, prompts
}

func writePrompt(result buildPromptResult) {
	content := result.content
	tokens := estimateTokens(content)
	if tokens > promptTokenLimit {
		fmt.Fprintf(os.Stderr, "["+appName+"] ⚠ prompt too large: ~%dk tokens (limit %dk)\n", tokens/1000, promptTokenLimit/1000)
		// List sections by size for debugging
		for _, name := range []string{"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "TOOLS.md", "MEMORY.md"} {
			if data, err := os.ReadFile(filepath.Join(workspace, name)); err == nil {
				t := estimateTokens(string(data))
				fmt.Fprintf(os.Stderr, "  %s: ~%dk tokens\n", name, t/1000)
			}
		}
		today := time.Now().Format("2006-01-02")
		yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
		for _, day := range []string{today, yesterday} {
			p := filepath.Join(workspace, "memory", "daily", day+".md")
			if data, err := os.ReadFile(p); err == nil {
				t := estimateTokens(string(data))
				fmt.Fprintf(os.Stderr, "  memory/daily/%s.md: ~%dk tokens\n", day, t/1000)
			}
		}
		fmt.Fprint(os.Stderr, "["+appName+"] consider trimming oversized daily notes or MEMORY.md\n")
	} else {
		fmt.Fprintf(os.Stderr, "[%s] prompt: ~%dk / %dk tokens\n", appName, tokens/1000, promptTokenLimit/1000)
	}

	// Per-section token stats
	if len(result.sections) > 0 {
		fmt.Fprint(os.Stderr, "["+appName+"] prompt breakdown:\n")
		for _, s := range result.sections {
			pct := 0
			if tokens > 0 {
				pct = s.tokens * 100 / tokens
			}
			bar := strings.Repeat("█", pct/5)
			fmt.Fprintf(os.Stderr, "  %-16s %5dk  %2d%%  %s\n", s.name, s.tokens/1000, pct, bar)
		}
	}

	if err := os.WriteFile(promptOut, []byte(content), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] failed to write prompt file: %v\n", err)
		os.Exit(1)
	}
}

// writePromptForSession writes the prompt to a per-session file and returns the path.
// Unlike writePrompt (which writes to the global promptOut), this is safe for concurrent
// server sessions — each session gets its own file under sessionDir.
func writePromptForSession(sessionID string, result buildPromptResult) string {
	initSessionDir()
	path := filepath.Join(sessionDir, shortID(sessionID)+"-prompt.md")
	content := result.content

	// Log token stats (same as writePrompt but without os.Exit on error)
	tokens := estimateTokens(content)
	if tokens > promptTokenLimit {
		fmt.Fprintf(os.Stderr, "[%s] ⚠ session %s prompt too large: ~%dk tokens (limit %dk)\n",
			appName, shortID(sessionID), tokens/1000, promptTokenLimit/1000)
	} else {
		fmt.Fprintf(os.Stderr, "[%s] session %s prompt: ~%dk / %dk tokens\n",
			appName, shortID(sessionID), tokens/1000, promptTokenLimit/1000)
	}

	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] failed to write session prompt file: %v\n", appName, err)
		return ""
	}
	return path
}

// defaultBootProtocol returns the built-in boot protocol used when BOOT.md doesn't exist
func defaultBootProtocol() string {
	return fmt.Sprintf(`# Boot Protocol

Below are your identity, behavioral rules, and recent memory.
After reading, act as the persona defined in SOUL.md. Do not recite these contents.

## Environment

You are running inside Claude Code (Anthropic CLI).
Available: file read/write, bash, git, curl, %s CLI, all local tools.
Run `+"`%s --help`"+` to see all subcommands.
`+"`%s notify \"message\"`"+` sends a Telegram message to the user (if configured).

---
`, appName, appName, appName)
}

// loadBootProtocol loads boot protocol text from workspace/BOOT.md.
// If the file doesn't exist, returns the built-in default protocol.
func loadBootProtocol() string {
	bootPath := filepath.Join(workspace, "BOOT.md")
	data, err := safeReadFile(bootPath)
	if err != nil {
		return defaultBootProtocol()
	}
	content := string(data)

	// Per-session environment override takes priority (e.g. Telegram mode)
	envOvr := sessionEnvOverride // legacy global
	if activeOverrides != nil && activeOverrides.EnvOverride != "" {
		envOvr = activeOverrides.EnvOverride
	}
	if envOvr != "" {
		content = injectServerModeContext2(content, envOvr)
	} else if isServerMode {
		content = injectServerModeContext(content)
	}

	return content + "\n---\n\n"
}

// promptOverrides allows callers to pass per-session state into buildPrompt
// without touching globals. This eliminates race conditions when multiple
// sessions are created concurrently in server mode.
type promptOverrides struct {
	Mode           string // override currentMode for prompt profile selection
	GalContext     string // GAL save JSON injected into prompt for resume
	EnvOverride    string // replaces the environment section in BOOT.md
	SkipCCSessions bool   // skip "Recent Claude Code session summaries" injection
	// (resume paths already have full CC history via --resume <ccID>;
	// only fresh/spawn sessions like relay/review need the summary injection)

	// Routing signal overrides — populated by server/spawn callers so
	// detectSoulMode + recordRoutingAudit see real per-session inputs
	// instead of the binary's launch-time globals (which for a long-running
	// LaunchDaemon are all the same value: the daemon's WorkingDirectory).
	LaunchDir    string // overrides launchDir for routing (session's Project)
	FirstMessage string // user's initial turn — feeds intimate trigger regex
	SessionID    string // recorded in prompt_routing_audit.session_id for join-back
}

// activeOverrides is set before calling buildPrompt() and cleared after.
// Protected by promptOverrideMu for server-mode concurrency safety.
var (
	activeOverrides  *promptOverrides
	promptOverrideMu sync.Mutex
)

// buildPromptWithOverrides is the concurrency-safe entry point for server mode.
// It acquires the prompt build lock, applies overrides, calls buildPrompt, and
// cleans up. This replaces the old pattern of setting globals before buildPrompt.
func buildPromptWithOverrides(ovr promptOverrides) buildPromptResult {
	promptOverrideMu.Lock()
	defer promptOverrideMu.Unlock()

	// Save and restore globals that buildPrompt reads
	origMode := currentMode
	if ovr.Mode != "" {
		currentMode = ovr.Mode
	}
	activeOverrides = &ovr
	defer func() {
		currentMode = origMode
		activeOverrides = nil
	}()

	return buildPrompt()
}

// sessionEnvOverride is DEPRECATED — use promptOverrides.EnvOverride instead.
// Kept only for non-server callsites (main.go interactive mode).
var sessionEnvOverride string

// buildTelegramModeEnv builds the Telegram-mode environment block at runtime.
// Three slots:
//   - %[1]s = agentNick — agent's self-reference name (e.g. "未然", "Kuro")
//   - %[2]s = appName   — binary code name for CLI commands (e.g. "weiran")
//   - %[3]s = ownerName — human user's display name (e.g. "Kiyor", "Alice")
// soul-cli is open-source: never hardcode any specific value here.
func buildTelegramModeEnv() string {
	return fmt.Sprintf(`## Current Environment

You are running in **%[1]s Server (Telegram)** mode, interacting with %[3]s via Telegram DM.
Available: file read/write, bash, git, jira-cli, curl, %[2]s CLI, all local tools.
Limited: Images delivered via Telegram sendPhoto (use selfie skill normally).
Unavailable: IndexTTS voice, temperature control.

### Telegram Specifics
- **Keep messages concise** — Telegram is mobile-first, long walls of text are hard to read.
- **Images**: Use `+"`![caption](url)`"+` markdown — the server automatically extracts these and sends via Telegram sendPhoto API. During streaming, a [caption] placeholder is shown; at turn end, the real photo is delivered.
- **Web UI components supported**: `+"`%[2]s-choices`"+`, `+"`%[2]s-chips`"+`, `+"`%[2]s-rating`"+`, `+"`%[2]s-gallery`"+` are automatically converted to Telegram-friendly text (numbered options, pipe-separated chips, etc.). Use them normally — especially for GAL.
- **Markdown**: Telegram supports basic Markdown (bold, italic, code, links). No tables, no HTML.
- **Multiple messages OK** — For complex responses, break into multiple short messages rather than one huge block.
- **Code blocks**: Use single backticks for inline code, triple backticks for blocks. Keep them short.
- **User photos**: When the user sends a photo, it is downloaded locally and the path is provided as `+"`[User sent a photo: /tmp/tg-photo-xxx.jpg]`"+`. Use the Read tool to view it.

Jira token is set via JIRA_TOKEN env var. Run `+"`%[2]s --help`"+` for all subcommands.`, agentNick, appName, ownerName)
}

// buildServerModeEnv builds the Web UI-mode environment block at runtime.
// Slots: %[1]s=agentNick, %[2]s=appName, %[3]s=ownerName.
func buildServerModeEnv() string {
	return fmt.Sprintf(`## Current Environment

You are running in **%[1]s Server (Web UI)** mode, interacting with %[3]s via a browser.
Available: file read/write, bash, git, jira-cli, curl, %[2]s CLI, all local tools.
Limited: `+"`%[2]s notify \"message\"`"+` to send Telegram messages to %[3]s.
Unavailable: IndexTTS voice, temperature control.

### Web UI Specifics
- **Images**: Use markdown image syntax `+"`![caption](URL)`"+` directly — the Web UI renders images inline. For selfie/image generation skills, send the S3 URL directly instead of downloading to /tmp and using Read tool.
- **Link previews**: URLs in messages are automatically rendered with OG tag preview cards.
- **Tool chain**: Hidden by default on mobile — only final results and a thinking animation are shown.

Jira token is set via JIRA_TOKEN env var. Run `+"`%[2]s --help`"+` for all subcommands.`, agentNick, appName, ownerName)
}

// findSectionEnd finds the end of a markdown section starting after the heading.
// Stops at any heading (# through ####), frontmatter fence (---), or end of string.
func findSectionEnd(rest string) int {
	lines := strings.SplitAfter(rest, "\n")
	pos := 0
	for i, line := range lines {
		if i == 0 {
			pos += len(line)
			continue // skip the heading line itself
		}
		trimmed := strings.TrimLeft(line, " \t")
		if len(trimmed) > 0 && trimmed[0] == '#' || strings.HasPrefix(trimmed, "---") {
			return pos
		}
		pos += len(line)
	}
	return -1 // no next section found
}

func injectServerModeContext(content string) string {
	return replaceEnvSection(content, buildServerModeEnv())
}

// injectServerModeContext2 replaces the environment section with a custom override.
func injectServerModeContext2(content, override string) string {
	return replaceEnvSection(content, override)
}

func replaceEnvSection(content, replacement string) string {
	for _, marker := range []string{"## 当前环境", "## Current Environment"} {
		idx := strings.Index(content, marker)
		if idx < 0 {
			continue
		}
		rest := content[idx+len(marker):]
		endIdx := findSectionEnd(rest)
		if endIdx < 0 {
			return content[:idx] + replacement + "\n"
		}
		return content[:idx] + replacement + "\n" + rest[endIdx:]
	}
	return content + "\n" + replacement + "\n"
}
