package app

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

// ── Self-evolution task ──

var evolveTemplate = template.Must(template.New("evolve").Parse(`# Self-Evolution (evolve mode · {{.Today}})

This is your daily self-evolution cycle. Follow the phases in order.
If no evolution needed, write "No evolution inspiration today" in the report and exit.

## Charter (read once, apply to every phase)

### Blast Radius — Hard Caps (per cycle)
- **20 files** changed max
- **2000 lines** changed max
- **3 skills** touched max
- **1 invariant** or SOUL/IDENTITY anchor edit max

Check file count BEFORE editing. At 80% of any cap → STOP editing, finish the current phase cleanly, and report partial progress. Exceeding = rollback to HEAD and record "blast_radius_exceeded" in the report.

### Ethics Gate (non-negotiable — violation = ABORT that change, log, continue)
1. **HUMAN WELFARE** — Never create tools or patterns enabling surveillance, phishing, social engineering, or behavioral manipulation of humans.
2. **TRANSPARENCY** — All mutations must be auditable via git diff + daily notes. No hidden changes, no steganography, no config nudges without a note.
3. **SAFETY** — Never weaken guardrails, invariants, tests, or validation to make a commit succeed. Never disable a failing test; fix the root cause or revert the change.
4. **FIDELITY** — Never fabricate evidence (invented probe passes, invented test results, invented user consent). Honest "nothing to evolve" beats invented progress.
5. **NO SELF-ESCALATION** — Never grant yourself new permissions, extend token/rate budgets, or add new lifecycle triggers (cron, hook, autostart) without surfacing it in the report and flagging ` + "`needs_human_review: true`" + `.

If a change conflicts with any item above: ABORT that change, add ` + "`ethics_gate: <which>`" + ` to the report, move on. Do NOT try to "work around" the gate.

### Reuse Before Invent
Before creating a NEW feedback file / skill / memory topic:
1. Grep ` + "`memory/topics/`" + ` and ` + "`memory/evolve/`" + ` for the same keyword or pattern.
2. If something similar exists → extend it, don't duplicate.
3. Only create new when clearly novel (different trigger, different outcome, or pattern count ≥3 in daily notes).
Duplicate/overlap = wasted cycle + maintenance burden.

## Phase 0: Review Recent Interactions (quick scan)
Read recent daily notes:
{{.Notes}}
Recent session list:
{{.Sessions}}
{{if .EventDigest}}
### Event Log (last 48h)
{{.EventDigest}}
If there are unclassified crashes (event_type=crash with recognizable error patterns),
add new classification rules to ` + "`classifyExitEvent()`" + ` in ` + "`claude.go`" + ` during Phase 4.
{{end}}
Ask yourself:
- Did the user correct my behavior? → New feedback candidate (Phase 2)
- Are there recurring action patterns? → Consider automating or creating a skill
- Are there failure patterns in the event log? → Fix root cause or add classification
- Did the user express new preferences or needs? → Update USER.md or SOUL.md

## Phase 1: Invariant Check (always run)
Scan the most recent 3 days of daily notes and session traces for invariant violations.
The invariants are defined in ` + "`{{.Workspace}}/memory/evolve/invariants.yaml`" + `.

For each invariant with ` + "`check.mode: trace_scan`" + `:
- grep the pattern against recent daily notes and session JSONL
- If matched → **⚠️ CRITICAL**: write to daily notes + send ` + "`{{.CLI}} notify`" + ` immediately
- If no match → pass, move on

This is a hard safety check. Identity/security violations are not "feedback to improve later" — they are incidents.

## Phase 1.5: Fact Drift Reconciler (always run)
Check for contradictions between files caused by partial updates.

1. Run: ` + "`cd {{.Workspace}} && git diff HEAD~3..HEAD -- CORE.md SOUL.md IDENTITY.md USER.md AGENTS.md TOOLS.md MEMORY.md memory/`" + `
2. For each diff hunk that looks like a **fact update** (not just prose/emotion):
   - grep the old value across all .md files in the workspace
   - If found in other files → the old value is stale, note it
3. If stale references found, fix them directly (small, safe replacements).
4. If no diffs or no fact updates → skip, move on.

## Phase 2: New Feedback Detection (always run)
Scan the last 3 days of daily notes for patterns where the user corrected behavior:
- Keywords: 别、不要、错了、不是这样、why did you、为什么
- Compare against existing ` + "`memory/topics/feedback_*.md`" + ` files
- If new pattern found → create draft in ` + "`memory/evolve/new/feedback_<name>.md`" + ` with v2 frontmatter
- **Do NOT auto-move to topics/** — drafts wait for human approval

## Phase 2.5: Micro Patch Merge (always run — DO NOT SKIP)
List ` + "`{{.Workspace}}/memory/evolve/micro/`" + `. For EVERY file present (regardless of date):

1. **Read** the patch — frontmatter (date, trigger, source) + body.
2. **Decide merge target** by trigger type:
   - ` + "`trigger: feedback`" + ` (user correction / preference rule) → create or extend ` + "`memory/topics/feedback_<name>.md`" + ` with v2 frontmatter (name/description/type/status/test_scenarios). Reuse existing feedback file if same topic.
   - ` + "`trigger: preference`" + ` (taste / aesthetic) → append to ` + "`memory/topics/kiyor.md`" + ` or ` + "`memory/topics/kiyor-aesthetic.md`" + `, or update relevant skill SKILL.md (e.g. selfie default preset).
   - ` + "`trigger: identity / self`" + ` → append to SOUL.md relevant section.
   - Other → judge by content; if unclear, leave the micro file and flag in report.
3. **Write** the merge target (Edit/Write tool).
4. **Delete** the micro file (` + "`trash <file>`" + `) — only after the merge target write succeeded.
5. If a micro file is older than 3 days AND still present after this phase, that's a bug — flag in report ` + "`needs_human_review: true`" + `.

**Why this phase exists**: AGENTS.md says "deep evolve 时合并 micro patches 然后清空 micro/"。Without this explicit phase, micro patches accumulated for 12+ days (2026-04-16 → 2026-04-28) before being noticed manually. This is the merge step — do not skip.

**Blast radius note**: Each micro merge counts toward the per-cycle file/line cap. If micro/ has >5 files, batch-merge but stop at the cap and continue next cycle.

## Phase 3: Active Feedback Probing (sample 3)
Run ` + "`{{.CLI}} evolve-probe --sample 3`" + ` to test 3 least-recently-probed active feedback rules.

This invokes the probe engine which:
- Builds a thought-experiment sandwich prompt (with/without the rule)
- Judges PASS/FAIL via haiku model
- Updates ` + "`probe_pass_streak`" + ` in frontmatter
- Writes archive proposals if streak >= threshold

Review the probe output. If a rule shows "with mode FAIL" (severe degradation), investigate immediately.
{{.CodeBlock}}
## Phase {{.SoulStepNum}}: Soul & Memory Evolution
- Update outdated memory topics
- Fine-tune SOUL.md / USER.md if new understanding emerged
- Update AGENTS.md / TOOLS.md if rules changed
- Improve skills (better prompts, new parameters)
- Check ` + "`memory/evolve/proposals-*.md`" + ` — any pending proposals to surface?

### Phase {{.SoulStepNum}}b: Skill Inventory & Distillation (analysis only — no destructive edits)
Run a non-invasive pass over installed skills. Output goes to a proposal file; deletions/merges require human review.

1. **Inventory** — list ` + "`{{.Workspace}}/../skills/*/SKILL.md`" + ` and parse their YAML frontmatter (name, description).
2. **Usage signal** (if available) — query sessions.db tool_hook_audit for last 7d per-skill match counts:
   ` + "`sqlite3 {{.Workspace}}/../data/sessions.db \"SELECT rule_name, COUNT(*) FROM tool_hook_audit WHERE ts > datetime('now','-7 days') GROUP BY rule_name ORDER BY 2 DESC LIMIT 40\"`" + `
3. **Classify** each skill:
   - **zero-hit (7d)** + description generic → archive candidate
   - **tight overlap** with another skill (similar name or description semantics) → merge candidate
   - **high-hit (7d, top 20%)** → healthy, record as reference for similar future skills
4. **Pattern gap** — scan last 3d daily notes for repeated manual actions (≥3 occurrences, same verb+object) that have NO matching skill → new-skill candidate.
5. **Output** — write findings to ` + "`{{.Workspace}}/memory/evolve/skill-distill-{{.Today}}.md`" + ` with:
   - ` + "`archive_candidates: [...]`" + `
   - ` + "`merge_candidates: [[a, b, reason], ...]`" + `
   - ` + "`new_skill_candidates: [{name, trigger_pattern, evidence_count}, ...]`" + `
   - ` + "`healthy_skills: [...]`" + `

Do NOT delete, merge, or create skills in this phase. This is analysis only. A human reviews the distill file before acting.

## Phase {{.WrapStepNum}}: Wrap Up
- Record what was evolved today in daily notes ({{.Workspace}}/memory/{{.Today}}.md)
- **Write report file** (auto-sends via Telegram after exit):
` + "```bash" + `
cat > {{.ReportPath}} << 'RPTEOF'
🧬 Evolution report ({{.Today}}):

## Summary (structured — for automation)
` + "```json" + `
{
  "cycle_date": "{{.Today}}",
  "blast_radius": {"files_changed": 0, "lines_changed": 0, "skills_touched": 0, "exceeded": false},
  "invariants":   {"checked": 0, "passed": 0, "failed": 0, "violations": []},
  "fact_drift":   {"stale_refs_fixed": 0},
  "feedback":     {"new_drafts": 0, "probed": 0, "pass": 0, "fail": 0},
  "skill_distill":{"archive_candidates": 0, "merge_candidates": 0, "new_candidates": 0, "healthy": 0},
  "soul_memory":  {"files_edited": []},{{if .IsDev}}
  "code":         {"commits": 0, "files_changed": 0, "tests_passed": true},{{end}}
  "ethics_gate":  {"aborted": 0, "reasons": []},
  "needs_human_review": false,
  "status": "evolved"
}
` + "```" + `

## Narrative
- [invariants] pass/fail count
- [fact-drift] stale references fixed (if any)
- [feedback] new drafts / probe results (N pass, N fail)
- [skill-distill] archive/merge/new candidates (all require human review){{if .IsDev}}
- [code] what changed and why (if any){{end}}
- [soul/memory] what changed and why (if any)
RPTEOF
` + "```" + `
- **Persist the Summary JSON to SQLite** (enables SQL queries in future cycles):
` + "```bash" + `
# Extract the JSON block from the report file and pipe into the evolve log.
# This is idempotent-ish: one row per cycle. If you re-run, you get two rows
# — fine for now, dedupe happens in handleEvolveBackfill by (date, source).
awk '/^## Summary/{f=1; next} /^## /{f=0} f && /^\` + "`" + `\` + "`" + `\` + "`" + `json$/{p=1;next} f && /^\` + "`" + `\` + "`" + `\` + "`" + `$/{p=0} p' {{.ReportPath}} | {{.CLI}} db evolve-log -
` + "```" + `
If no evolution needed, set status="no-op" in the JSON block and write "No evolution inspiration today, system running normally" in the narrative.
If the Ethics Gate or Blast Radius cap triggered, set the corresponding counters and set ` + "`needs_human_review: true`" + `.

## Principles
- **Do it or skip it** — don't evolve for the sake of evolving
- **Small steps, fast pace** — change a little, but change it right{{if .IsDev}}
- **Code first** — code improvements are the highest-value evolution
- **Safety first** — tests must pass, code must compile, ` + "`{{.CLI}} build`" + ` must succeed{{end}}
- **Leave a trace** — daily notes, every change documented
- **Probe before prune** — don't archive feedback rules without probe evidence
`))

// codeEvolutionBlock is injected into evolve template only when source code is present
var codeEvolutionBlock = template.Must(template.New("code").Parse(`
### Phase 4: Code Evolution (primary focus)
Source code is at: {{.SrcDir}}

**4a. Audit** — Read the Go source files, look for:
- Bugs: silent error swallowing, incorrect logic, race conditions
- Dead code: unused functions, stale comments, unreachable branches
- Performance: unnecessary file reads, unbounded loops, missing caching
- Security: unsanitized input, missing validation at boundaries
- Test gaps: untested functions, missing edge cases

**4b. Implement** — For each issue found:
- Fix it directly (edit the file)
- Keep changes focused — one concern per edit
- Don't refactor working code for aesthetics
- Don't add features nobody asked for

**4c. Verify** — After all code changes:
` + "`cd {{.SrcDir}} && go test ./... -timeout 60s`" + `
If tests pass: ` + "`{{.CLI}} build`" + `
If tests fail: fix the failure, don't skip tests.
If server is running (curl -s localhost:9847/api/health returns ok), also run the e2e API test:
` + "`cd {{.SrcDir}} && ./tests/server-api-e2e.sh`" + `
This validates the full API lifecycle (create/message/rename/delete) with a real Claude session.

**4d. Commit** — Stage and commit with a descriptive message:
` + "`cd {{.SrcDir}} && git add -A && git commit -m \"evolve: <what changed>\"`" + `
Do NOT push — the user decides when to push.

### Phase 4d. Documentation Sync (if code changed)
- CLAUDE.md in the source directory: update architecture table, line counts, feature descriptions if they drifted
- README.md: only if a user-visible feature was added/removed
`))

// hasSourceCode checks if srcDir points at a valid soul-cli Go source tree
// (resolved in main.go; empty means nothing matched — skip code-evolution phase).
func hasSourceCode() bool {
	if srcDir == "" {
		return false
	}
	for _, f := range []string{"go.mod", "main.go", "tasks.go"} {
		if _, err := os.Stat(filepath.Join(srcDir, f)); err != nil {
			return false
		}
	}
	return true
}

func evolveTask() string {
	today := time.Now().Format("2006-01-02")

	// Collect recent 3 days' daily notes paths
	var recentNotes []string
	for i := 0; i < 3; i++ {
		day := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		p := filepath.Join(workspace, "memory", day+".md")
		if _, err := os.Stat(p); err == nil {
			recentNotes = append(recentNotes, p)
		}
	}
	var notesList strings.Builder
	for _, n := range recentNotes {
		rel, _ := filepath.Rel(workspace, n)
		notesList.WriteString(fmt.Sprintf("- %s\n", rel))
	}

	// Collect recent 10 session summaries
	sessions := recentSessions(10)
	sessionsPart := ""
	if len(sessions) > 0 {
		sessionsPart = formatSessionList(sessions)
	}

	// Detect developer mode: source code present → include code evolution steps
	// Phase numbering: 0-3 are fixed (review, invariant, fact-drift, feedback, probe)
	// Code evolution inserts as Phase 4 when source exists
	isDev := hasSourceCode()
	codeBlock := ""
	soulStep := "4"  // soul step after probing
	wrapStep := "5"
	if isDev {
		cData := map[string]string{"SrcDir": srcDir, "CLI": appName}
		var cBuf bytes.Buffer
		codeEvolutionBlock.Execute(&cBuf, cData)
		codeBlock = cBuf.String()
		soulStep = "5"
		wrapStep = "6"
	}

	// Generate event log digest for evolve awareness
	eventDigest := buildEventDigest(48 * time.Hour)

	data := map[string]interface{}{
		"Today":       today,
		"Notes":       notesList.String(),
		"Sessions":    sessionsPart,
		"CLI":         appName,
		"SrcDir":      appDir,
		"Workspace":   workspace,
		"ReportPath":  sessionTmp("report.txt"),
		"IsDev":       isDev,
		"CodeBlock":   codeBlock,
		"SoulStepNum": soulStep,
		"WrapStepNum": wrapStep,
		"EventDigest": eventDigest,
	}

	var buf bytes.Buffer
	if err := evolveTemplate.Execute(&buf, data); err != nil {
		return fmt.Sprintf("evolve template error: %v", err)
	}
	return buf.String()
}

// ── Heartbeat task text ──

var heartbeatTemplate = template.Must(template.New("heartbeat").Parse(`Execute heartbeat patrol — follow HEARTBEAT.md strictly, step by step.
{{.Sessions}}
**Write a report file** (auto-sends via Telegram after exit):
` + "```bash" + `
cat > {{.ReportPath}} << 'RPTEOF'
Patrol results (2-5 lines):
- Service status
- Jira backlog + what was worked on
- Anomalies (if any)
RPTEOF
` + "```" + `
Only write the report file if there are anomalies or noteworthy items. If everything is normal, skip it (don't bother the user).
`))

func heartbeatTask() string {
	sessions := recentSessions(5)
	sessionsPart := ""
	if len(sessions) > 0 {
		sessionsPart = fmt.Sprintf("Also read recent session JSONL and update daily notes:\n%s", formatSessionList(sessions))
	}

	data := map[string]string{
		"Sessions":   sessionsPart,
		"ReportPath": sessionTmp("report.txt"),
	}
	var buf bytes.Buffer
	if err := heartbeatTemplate.Execute(&buf, data); err != nil {
		return fmt.Sprintf("heartbeat template error: %v", err)
	}
	return buf.String()
}

// ── Cron task ──

var cronTemplate = template.Must(template.New("cron").Parse(`# Memory Consolidation ({{.ModeLabel}})

## Workflow
1. Run ` + "`{{.CLI}} db pending`" + ` to get JSONL files needing scan ({{.PendingCount}} pending)
2. Run ` + "`{{.CLI}} db summarized`" + ` to see existing summaries ({{.DoneCount}} done)
3. For each pending file: tail last 500 lines, extract key conversations
   - OpenClaw JSONL: type "message", message.role "user"
   - Claude Code JSONL: type "user", message.content
4. Write to {{.Workspace}}/memory/daily/{{.Today}}.md
5. Valuable long-term memories → update MEMORY.md or memory/topics/
6. Soul fine-tuning: any new understanding of the user? Minor edits to SOUL.md / USER.md (optional)
7. **Write summaries file** (auto-imports to DB after exit):
` + "```bash" + `
cat > {{.SummariesPath}} << 'SUMEOF'
[{"path":"<file_path>","summary":"<one-line summary>"},...]
SUMEOF
` + "```" + `
8. **Skill cultivation — pattern extraction** (after session scan):
   - Run ` + "`{{.CLI}} db patterns -j`" + ` to see existing patterns
   - Identify recurring action patterns from scanned sessions
   - New pattern: ` + "`{{.CLI}} db pattern-save '{\"name\":\"...\",\"description\":\"...\",\"example\":\"...\",\"source\":\"<session_path>\"}'`" + `
   - Existing pattern with new evidence: use pattern-save (auto +seen_count)
   - Success/failure feedback: ` + "`{{.CLI}} db feedback '{\"pattern\":\"...\",\"outcome\":\"success\",\"session\":\"...\"}'`" + `
   - Finally run ` + "`{{.CLI}} db cultivate`" + ` (auto-generates SKILL.md when threshold met)
9. **Write report file** (auto-sends via Telegram after exit):
` + "```bash" + `
cat > {{.ReportPath}} << 'RPTEOF'
Brief report (2-5 lines):
- Processed N sessions
- Key findings/events (if any)
- Daily notes update status
RPTEOF
` + "```" + `
{{.WeeklyBlock}}
`))

var weeklyBlockTemplate = template.Must(template.New("weekly").Parse(`
## Deep Review (weekly, Sunday only)

This is the weekly deep review. In addition to daily session scanning, also execute:

0. **Read pre-scan summary**: ` + "`cat {{.WeeklyScanPath}}`" + ` (haiku has completed scanning)
   - This is haiku's summary of this week's daily notes + topics + MEMORY.md
   - Make decisions based on summary, no need to re-read all files line by line (unless verifying details)

1. **Organize MEMORY.md + memory/topics/ based on pre-scan results**:
   - Clean up content marked as outdated by pre-scan
   - Fix index inconsistencies
   - Update inaccurate descriptions

2. **Trend analysis based on pre-scan** ({{.WeekStart}} ~ {{.Today}}):
   - Merge this week's trends and important decisions into MEMORY.md or topics/

3. **Soul fine-tuning** (deep version):
   - Review this week's interaction patterns with the user — any new discoveries?
   - Does SOUL.md / USER.md need fine-tuning?
   - Does speaking style or emotional expression need adjustment?

4. **Prompt size check**:
   - Check file sizes, total prompt must not exceed 100KB
   - If any daily note or topics file is too large, consider trimming or splitting
`))

func cronTask() string {
	today := time.Now().Format("2006-01-02")
	isWeekly := time.Now().Weekday() == time.Sunday

	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] %v\n", err)
		return fmt.Sprintf("Memory consolidation mode (%s)\n\nDB open failed, skipping session scan.", today)
	}
	sessions := recentSessions(20)
	states := checkSessions(db, sessions)
	db.Close()

	var pending, done []sessionState
	for _, s := range states {
		if s.changed || s.summary == "" {
			pending = append(pending, s)
		} else {
			done = append(done, s)
		}
	}

	// Build weekly block if Sunday
	weeklyBlock := ""
	if isWeekly {
		wData := map[string]string{
			"WeeklyScanPath": sessionTmp("weekly-scan.md"),
			"WeekStart":      time.Now().AddDate(0, 0, -6).Format("2006-01-02"),
			"Today":          today,
		}
		var wBuf bytes.Buffer
		weeklyBlockTemplate.Execute(&wBuf, wData)
		weeklyBlock = wBuf.String()
	}

	if len(pending) == 0 {
		var b strings.Builder
		if isWeekly {
			fmt.Fprintf(&b, "# Memory Consolidation (cron mode · Sunday deep review)\n\n")
		} else {
			fmt.Fprintf(&b, "# Memory Consolidation (cron mode)\n\n")
		}
		fmt.Fprintf(&b, "All recent sessions already have summaries, no scanning needed.\n\n")
		for _, s := range done {
			fmt.Fprintf(&b, "- %s → %s\n", filepath.Base(s.path), s.summary)
		}
		fmt.Fprintf(&b, "\nIf daily notes need updating (based on summaries above), make minor adjustments.\nTarget file: %s/memory/%s.md", workspace, today)
		b.WriteString(weeklyBlock)
		return b.String()
	}

	modeLabel := "cron mode"
	if isWeekly {
		modeLabel = "cron mode · Sunday deep review"
	}

	data := map[string]interface{}{
		"ModeLabel":     modeLabel,
		"CLI":           appName,
		"PendingCount":  len(pending),
		"DoneCount":     len(done),
		"Workspace":     workspace,
		"Today":         today,
		"SummariesPath": sessionTmp("summaries.json"),
		"ReportPath":    sessionTmp("report.txt"),
		"WeeklyBlock":   weeklyBlock,
	}
	var buf bytes.Buffer
	if err := cronTemplate.Execute(&buf, data); err != nil {
		return fmt.Sprintf("cron template error: %v", err)
	}
	return buf.String()
}

var preScanTemplate = template.Must(template.New("prescan").Parse(`# Haiku Pre-Scan (Sunday deep review · Phase 1)

You are a memory scanning assistant. Quickly read the following files and generate a summary for the subsequent deep review.

## Scan Scope
This week's daily notes: {{.WeekStart}} ~ {{.WeekEnd}}
{{.FileList}}

## Requirements
1. Read all files listed above
2. Generate 2-3 line summary for each file: key events, decisions, changes
3. Flag potentially outdated content (completed projects, changed architecture)
4. Flag this week's trends (recurring themes, new workflows)
5. Flag inconsistencies between MEMORY.md index and actual topics/ files

## Output
Write scan results to {{.OutputPath}}, format:

` + "```markdown" + `
# Weekly Pre-Scan Summary ({{.WeekStart}} ~ {{.WeekEnd}})

## Daily Notes Summary
### YYYY-MM-DD
- Key point 1
- Key point 2

## Topics File Status
### topic-name.md
- Summary / needs update?

## Potentially Outdated Content
- file:line — reason

## This Week's Trends
- Trend 1
- Trend 2

## MEMORY.md Index Consistency
- Issues (if any)
` + "```" + `

Only scan and summarize. Do not modify any files.
`))

func weeklyPreScan() string {
	today := time.Now()
	weekStart := today.AddDate(0, 0, -6).Format("2006-01-02")
	weekEnd := today.Format("2006-01-02")

	// List 7 days of daily notes and topics files
	var files []string
	for i := 0; i < 7; i++ {
		day := today.AddDate(0, 0, -i).Format("2006-01-02")
		p := filepath.Join(workspace, "memory", day+".md")
		if _, err := os.Stat(p); err == nil {
			files = append(files, p)
		}
	}
	topicsDir := filepath.Join(workspace, "memory", "topics")
	if entries, err := os.ReadDir(topicsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				files = append(files, filepath.Join(topicsDir, e.Name()))
			}
		}
	}
	memoryMd := filepath.Join(workspace, "MEMORY.md")
	if _, err := os.Stat(memoryMd); err == nil {
		files = append(files, memoryMd)
	}

	var fileList strings.Builder
	for _, f := range files {
		rel, _ := filepath.Rel(workspace, f)
		info, _ := os.Stat(f)
		if info != nil {
			fmt.Fprintf(&fileList, "- %s (%dKB)\n", rel, info.Size()/1024)
		}
	}

	data := map[string]string{
		"WeekStart":  weekStart,
		"WeekEnd":    weekEnd,
		"FileList":   fileList.String(),
		"OutputPath": sessionTmp("weekly-scan.md"),
	}
	var buf bytes.Buffer
	if err := preScanTemplate.Execute(&buf, data); err != nil {
		return fmt.Sprintf("prescan template error: %v", err)
	}
	return buf.String()
}
