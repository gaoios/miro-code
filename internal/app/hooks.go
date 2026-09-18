package app

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// hooksDir is set in initWorkspace
var hooksDir string

// hookRecord records the execution result of a single hook
type hookRecord struct {
	Name     string  `json:"name"`
	ExitCode int     `json:"exit_code"`
	Duration float64 `json:"duration_s"`
	Error    string  `json:"error,omitempty"`
}

func runHooks(mode string) {
	fmt.Fprintf(os.Stderr, "["+appName+"] running post-hooks (%s)\n", mode)
	var journal []hookRecord

	// built-in hook 1: import summaries
	t0 := time.Now()
	importSummaries()
	journal = append(journal, hookRecord{Name: "importSummaries", Duration: time.Since(t0).Seconds()})

	// built-in hook 1.5: import patterns (cron mode)
	if mode == "cron" {
		t0 = time.Now()
		importPatterns()
		journal = append(journal, hookRecord{Name: "importPatterns", Duration: time.Since(t0).Seconds()})
	}

	// built-in hook 2: send report to Telegram
	t0 = time.Now()
	deliverReport(mode)
	journal = append(journal, hookRecord{Name: "deliverReport", Duration: time.Since(t0).Seconds()})

	// built-in hook 3: post-session safety check
	t0 = time.Now()
	safetyCheck(mode)
	journal = append(journal, hookRecord{Name: "safetyCheck", Duration: time.Since(t0).Seconds()})

	// built-in hook 4: auto-clean old temp directories (>24h)
	t0 = time.Now()
	autoCleanTmp()
	journal = append(journal, hookRecord{Name: "autoCleanTmp", Duration: time.Since(t0).Seconds()})

	// built-in hook 5: incremental FTS5 index (daily notes + session content)
	t0 = time.Now()
	if n, s, err := indexDailyNotes(); err == nil {
		_ = s
		journal = append(journal, hookRecord{Name: fmt.Sprintf("ftsIndexNotes(%d)", n), Duration: time.Since(t0).Seconds()})
	}
	t0 = time.Now()
	if n, s, err := indexSessionContent(); err == nil {
		_ = s
		journal = append(journal, hookRecord{Name: fmt.Sprintf("ftsIndexSessions(%d)", n), Duration: time.Since(t0).Seconds()})
	}

	// user-defined hooks: hooks/{mode}.d/*.sh executed in filename order
	hookDir := filepath.Join(hooksDir, mode+".d")
	entries, err := os.ReadDir(hookDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".sh") {
				continue
			}
			script := filepath.Join(hookDir, e.Name())
			fmt.Fprintf(os.Stderr, "["+appName+"] hook: %s\n", e.Name())
			t0 = time.Now()
			cmd := exec.Command("bash", script)
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr

			upperName := strings.ToUpper(strings.ReplaceAll(appName, "-", "_"))
			cmd.Env = append(os.Environ(),
				upperName+"_MODE="+mode,
				upperName+"_WORKSPACE="+workspace,
				upperName+"_DB="+dbPath,
				upperName+"_SESSION_DIR="+sessionDir,
			)
			rec := hookRecord{Name: e.Name()}
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "["+appName+"] hook %s failed: %v\n", e.Name(), err)
				rec.Error = err.Error()
				rec.ExitCode = 1
				if exitErr, ok := err.(*exec.ExitError); ok {
					rec.ExitCode = exitErr.ExitCode()
				}
			}
			rec.Duration = time.Since(t0).Seconds()
			journal = append(journal, rec)
		}
	}

	// write hook execution journal
	writeHookJournal(journal)

	// consecutive failure detection — read metrics.jsonl, alert on 3+ consecutive non-zero exits for same mode
	checkFailureStreak(mode)
}

func writeHookJournal(journal []hookRecord) {
	if sessionDir == "" {
		return
	}
	path := filepath.Join(sessionDir, "hooks.json")
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(path, data, 0644)
	fmt.Fprintf(os.Stderr, "["+appName+"] hook journal: %s\n", path)
}

// checkFailureStreak reads metrics.jsonl and detects consecutive failures for the same mode
func checkFailureStreak(mode string) {
	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	data, err := os.ReadFile(metricsPath)
	if err != nil {
		return
	}

	// read recent records for this mode in reverse order
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	streak := 0
	for i := len(lines) - 1; i >= 0; i-- {
		var rec struct {
			Mode     string `json:"mode"`
			ExitCode int    `json:"exit_code"`
		}
		if json.Unmarshal([]byte(lines[i]), &rec) != nil {
			continue
		}
		if rec.Mode != mode {
			continue
		}
		if rec.ExitCode != 0 {
			streak++
		} else {
			break // stop at first success
		}
		if streak >= 5 {
			break // check at most 5 entries
		}
	}

	if streak >= 3 {
		msg := fmt.Sprintf("⚠️ %s mode failed %d times consecutively, possible systemic issue", mode, streak)
		fmt.Fprintf(os.Stderr, "["+appName+"] %s\n", msg)

		// auto-create Jira ticket
		jiraCmd := exec.Command("jira-cli", "create",
			fmt.Sprintf("%s failed %d times consecutively — needs diagnosis", mode, streak),
			"--priority", "high",
			"--description", fmt.Sprintf("%s %s mode had %d consecutive non-zero exits. Check metrics.jsonl and recent session logs.", appName, mode, streak),
		)
		if out, err := jiraCmd.CombinedOutput(); err != nil {
			// jira-cli failure doesn't affect main flow, fall through to Telegram alert
			fmt.Fprintf(os.Stderr, "["+appName+"] jira ticket creation failed: %v: %s\n", err, out)
		}

		// Telegram alert
		trySendTelegram(fmt.Sprintf("🚨 %s\n\nmetrics.jsonl shows the last %d runs of %s all exited non-zero, please investigate.", msg, streak, mode))
	}
}

// ── Safety Check ──

// safetyCheck runs integrity checks after each cron/heartbeat session
// detects: soul file loss, memory bloat/wipe, config drift, sensitive info leaks
func safetyCheck(mode string) {
	fmt.Fprint(os.Stderr, "["+appName+"] safety check starting\n")
	var warnings []string

	// 1. soul file existence — losing these is catastrophic
	// use Lstat to detect symlinks: soul files replaced by symlinks are treated as tampering
	soulFiles := []string{
		"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md",
		"TOOLS.md", "MEMORY.md", "HEARTBEAT.md",
	}
	for _, f := range soulFiles {
		path := filepath.Join(workspace, f)
		info, err := os.Lstat(path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("🚨 soul file missing: %s", f))
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			warnings = append(warnings, fmt.Sprintf("🚨 soul file replaced by symlink: %s → possible tampering", f))
			continue
		}
		if info.Size() == 0 {
			warnings = append(warnings, fmt.Sprintf("🚨 soul file emptied: %s", f))
		}
	}

	// 1.5. CORE.md integrity — auto-restore if modified
	corePath := filepath.Join(workspace, "CORE.md")
	coreCommitted, coreErr := exec.Command("git", "-C", workspace, "show", "HEAD:CORE.md").Output()
	if coreErr == nil {
		currentCore, readErr := os.ReadFile(corePath)
		if readErr != nil || string(currentCore) != string(coreCommitted) {
			// Auto-restore from git
			if restoreErr := os.WriteFile(corePath, coreCommitted, 0644); restoreErr == nil {
				warnings = append(warnings, "🚨 CORE.md was modified — auto-restored from git. This file is read-only for agents.")
			} else {
				warnings = append(warnings, fmt.Sprintf("🚨 CORE.md was modified and auto-restore FAILED: %v", restoreErr))
			}
		}
	}

	// 1.6. soul file content shrinkage detection — compare working tree vs last commit
	// Prevents "optimization" that silently deletes personality/ability/relationship content
	protectedFiles := []string{"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "BOOT.md"}
	for _, f := range protectedFiles {
		currentPath := filepath.Join(workspace, f)
		currentData, err := os.ReadFile(currentPath)
		if err != nil {
			continue
		}
		// Get committed version size via git
		committed, err := exec.Command("git", "-C", workspace, "show", "HEAD:"+f).Output()
		if err != nil {
			continue // file not tracked or no commits
		}
		currentSize := len(currentData)
		committedSize := len(committed)
		if committedSize == 0 {
			continue
		}
		shrinkPct := float64(committedSize-currentSize) / float64(committedSize) * 100
		if shrinkPct > 20 {
			warnings = append(warnings, fmt.Sprintf("🚨 soul file shrank %.0f%%: %s (%d → %d bytes) — content may have been over-trimmed", shrinkPct, f, committedSize, currentSize))
		}
	}

	// 2. memory file sanity check — bloat or wipe
	memDir := filepath.Join(workspace, "memory")
	if entries, err := os.ReadDir(memDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.Size() > 100*1024 { // > 100KB
				warnings = append(warnings, fmt.Sprintf("⚠️ memory file bloat: %s (%dKB)", e.Name(), info.Size()/1024))
			}
			if info.Size() == 0 {
				warnings = append(warnings, fmt.Sprintf("⚠️ memory file emptied: %s", e.Name()))
			}
		}
		// topics subdirectory
		topicsDir := filepath.Join(memDir, "topics")
		if topicEntries, err := os.ReadDir(topicsDir); err == nil {
			for _, e := range topicEntries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				if info.Size() > 100*1024 {
					warnings = append(warnings, fmt.Sprintf("⚠️ memory file bloat: topics/%s (%dKB)", e.Name(), info.Size()/1024))
				}
				if info.Size() == 0 {
					warnings = append(warnings, fmt.Sprintf("⚠️ memory file emptied: topics/%s", e.Name()))
				}
			}
		}
	}

	// 3. sensitive info leak detection — scan git tracked file diffs
	sensitivePatterns := []string{
		"botToken", "ANTHROPIC_API_KEY", "OPENAI_API_KEY",
		"sk-ant-", "sk-proj-", "Bearer ", "secret_key",
		"AKIA", "aws_secret", "ghp_", "gho_", "github_pat_",
		"xoxb-", "xoxp-", "PRIVATE KEY", "password",
	}
	// Only check staged diff (about-to-commit) — unstaged diff in docs (TOOLS.md / topics/feedback_*.md /
	// CLAUDE.md) frequently mentions Bearer/password/token as concepts and produced 100% false-positive rate.
	// pathspec excludes markdown docs since real secrets should never live in *.md files anyway.
	gitDiff, err := exec.Command(
		"git", "-C", workspace, "diff", "--cached", "--diff-filter=ACMR",
		"--", ".", ":(exclude)*.md", ":(exclude)**/*.md",
	).Output()
	if err == nil && len(gitDiff) > 0 {
		diffStr := string(gitDiff)
		for _, pat := range sensitivePatterns {
			if strings.Contains(diffStr, pat) {
				warnings = append(warnings, fmt.Sprintf("🔐 sensitive pattern detected in staged diff: %s", pat))
			}
		}
	}

	// 4. critical config file change detection — check uncommitted changes + show diff summary
	// nginx was archived 2026-04-24, replaced by mac-host caddy. Caddyfile is the new critical config.
	criticalFiles := []string{
		"projects/caddy/Caddyfile",
	}
	gitStatus, err := exec.Command("git", "-C", workspace, "status", "--porcelain").Output()
	if err == nil {
		statusStr := string(gitStatus)
		for _, f := range criticalFiles {
			if strings.Contains(statusStr, f) {
				// get diff summary (up to 10 lines)
				diffOut, diffErr := exec.Command("git", "-C", workspace, "diff", "--unified=0", f).Output()
				diffSummary := ""
				if diffErr == nil && len(diffOut) > 0 {
					lines := strings.Split(string(diffOut), "\n")
					limit := 10
					if len(lines) < limit {
						limit = len(lines)
					}
					diffSummary = "\n```\n" + strings.Join(lines[:limit], "\n") + "\n```"
				}
				warnings = append(warnings, fmt.Sprintf("⚙️ critical config file has uncommitted changes: %s%s", f, diffSummary))
			}
		}
	}

	// 5. Docker service config integrity — check docker-compose.yml not deleted
	projectsDir := filepath.Join(workspace, "projects")
	if entries, err := os.ReadDir(projectsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			composePath := filepath.Join(projectsDir, e.Name(), "docker-compose.yml")
			if _, err := os.Stat(composePath); err == nil {
				// compose file exists, check size
				info, _ := os.Stat(composePath)
				if info != nil && info.Size() == 0 {
					warnings = append(warnings, fmt.Sprintf("⚙️ docker-compose.yml emptied: projects/%s/", e.Name()))
				}
			}
		}
	}

	// 6. Markdown format validation (topics, skills, CLAUDE.md)
	warnings = append(warnings, validateMdFormats()...)

	// summary
	if len(warnings) == 0 {
		fmt.Fprint(os.Stderr, "["+appName+"] safety check passed ✓\n")
		return
	}

	// issues found: output to stderr + send Telegram alert
	fmt.Fprintf(os.Stderr, "["+appName+"] safety check found %d issues:\n", len(warnings))
	var report strings.Builder
	report.WriteString(fmt.Sprintf("🛡️ *Safety Check — %s*\n\n", mode))
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "  %s\n", w)
		report.WriteString(w + "\n")
	}
	report.WriteString(fmt.Sprintf("\n_checked at: %s_", time.Now().Format("2006-01-02 15:04")))

	// Always persist full report to disk so we never lose it to TG length limits.
	reportsDir := filepath.Join(workspace, "memory", "safety-reports")
	_ = os.MkdirAll(reportsDir, 0755)
	reportPath := filepath.Join(reportsDir, fmt.Sprintf("%s-%s.md", time.Now().Format("2006-01-02-150405"), mode))
	_ = os.WriteFile(reportPath, []byte(report.String()), 0644)

	// Telegram has a 4096-char message cap. If we'd exceed ~3500, send a summary + path to full report.
	const tgSoftLimit = 3500
	tgMsg := report.String()
	if len(tgMsg) > tgSoftLimit {
		// keep the header + first N lines that fit, then append a tail pointing to the full report
		var summary strings.Builder
		summary.WriteString(fmt.Sprintf("🛡️ *Safety Check — %s* (%d issues, truncated)\n\n", mode, len(warnings)))
		shown := 0
		for _, w := range warnings {
			line := w + "\n"
			if summary.Len()+len(line)+200 > tgSoftLimit {
				break
			}
			summary.WriteString(line)
			shown++
		}
		if shown < len(warnings) {
			summary.WriteString(fmt.Sprintf("\n…and %d more issue(s).\n", len(warnings)-shown))
		}
		summary.WriteString(fmt.Sprintf("\nFull report: `%s`\n_checked at: %s_", reportPath, time.Now().Format("2006-01-02 15:04")))
		tgMsg = summary.String()
	}

	if err := trySendTelegram(tgMsg); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] safety check alert send failed: %v (full report at %s)\n", err, reportPath)
	}
}

// autoCleanTmp cleans up session temp directories older than 24 hours
func autoCleanTmp() {
	cutoff := time.Now().Add(-24 * time.Hour)
	tmpDirs := findAppTmpDirs()
	cleaned := 0
	for _, d := range tmpDirs {
		// don't clean current session's directory
		if d == sessionDir {
			continue
		}
		info, err := os.Stat(d)
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			continue
		}
		cleaned++
	}
	if cleaned > 0 {
		fmt.Fprintf(os.Stderr, "["+appName+"] auto-cleaned %d old temp directories\n", cleaned)
	}
}

// deliverReport reads the report file written by claude and sends it via Telegram
func deliverReport(mode string) {
	data, err := os.ReadFile(sessionTmp("report.txt"))
	if err != nil {
		fmt.Fprint(os.Stderr, "["+appName+"] no report file, skipping delivery\n")
		return
	}

	report := strings.TrimSpace(string(data))
	if report == "" {
		return
	}

	// add mode prefix
	prefix := map[string]string{
		"cron":      "📓",
		"heartbeat": "💓",
		"evolve":    "🧬",
	}
	emoji := prefix[mode]
	if emoji == "" {
		emoji = "📋"
	}
	msg := fmt.Sprintf("%s *%s report*\n\n%s", emoji, mode, report)

	if err := trySendTelegram(msg); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] report send failed: %v\n", err)
		return
	}
	fmt.Fprint(os.Stderr, "["+appName+"] report sent to Telegram\n")
}

// parseMdFrontmatter extracts key-value pairs from YAML frontmatter in a markdown file.
// Returns nil if no frontmatter found.
func parseMdFrontmatter(content string) map[string]string {
	if !strings.HasPrefix(content, "---") {
		return nil
	}
	end := strings.Index(content[3:], "---")
	if end < 0 {
		return nil
	}
	result := make(map[string]string)
	for _, line := range strings.Split(content[3:3+end], "\n") {
		line = strings.TrimSpace(line)
		if idx := strings.Index(line, ":"); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])
			val = strings.Trim(val, "\"'")
			if key != "" {
				result[key] = val
			}
		}
	}
	return result
}

// validateMdFormats validates frontmatter/format of all structured md files:
// memory topics, skills, project CLAUDE.md files, and core soul files.
func validateMdFormats() []string {
	var warnings []string

	// 0. CORE.md integrity check
	corePath := filepath.Join(workspace, "CORE.md")
	coreCommitted, coreErr := exec.Command("git", "-C", workspace, "show", "HEAD:CORE.md").Output()
	if coreErr == nil {
		currentCore, readErr := os.ReadFile(corePath)
		if readErr != nil {
			warnings = append(warnings, "📝 CORE.md missing or unreadable")
		} else if string(currentCore) != string(coreCommitted) {
			warnings = append(warnings, "📝 CORE.md differs from committed version (should be read-only for agents)")
		}
	}

	// 1. Core soul files: existence, non-empty, no cross-file duplication
	soulFiles := map[string]int64{} // path → size
	for _, name := range []string{"BOOT.md", "SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "TOOLS.md", "MEMORY.md"} {
		path := filepath.Join(workspace, name)
		info, err := os.Stat(path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("📝 missing soul file: %s", name))
		} else if info.Size() == 0 {
			warnings = append(warnings, fmt.Sprintf("📝 empty soul file: %s", name))
		} else {
			soulFiles[name] = info.Size()
		}
	}

	// Check MEMORY.md index consistency: entries should point to existing files
	memoryMdPath := filepath.Join(workspace, "MEMORY.md")
	if data, err := os.ReadFile(memoryMdPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			// Match markdown links like [Title](memory/topics/foo.md)
			if idx := strings.Index(line, "](memory/topics/"); idx > 0 {
				endIdx := strings.Index(line[idx+2:], ")")
				if endIdx > 0 {
					relPath := line[idx+2 : idx+2+endIdx]
					fullPath := filepath.Join(workspace, relPath)
					if _, err := os.Stat(fullPath); os.IsNotExist(err) {
						warnings = append(warnings, fmt.Sprintf("📝 MEMORY.md broken link: %s (file not found)", relPath))
					}
				}
			}
		}
	}

	// 2. Memory topic files: must have frontmatter with name, description, type
	// type is now free-form — we record what kind of topic it is, but don't restrict the vocabulary.
	// Previously enforced {feedback|user|project|reference} which conflicted with naturally-evolving
	// genres like analysis / workflow / config-note / production-decision.
	topicsDir := filepath.Join(workspace, "memory", "topics")
	if entries, err := os.ReadDir(topicsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			label := "topics/" + e.Name()
			data, err := os.ReadFile(filepath.Join(topicsDir, e.Name()))
			if err != nil {
				continue
			}
			fm := parseMdFrontmatter(string(data))
			if fm == nil {
				warnings = append(warnings, fmt.Sprintf("📝 missing frontmatter: %s", label))
				continue
			}
			if fm["name"] == "" {
				warnings = append(warnings, fmt.Sprintf("📝 missing name: %s", label))
			}
			if fm["description"] == "" {
				warnings = append(warnings, fmt.Sprintf("📝 missing description: %s", label))
			}
			if fm["type"] == "" {
				warnings = append(warnings, fmt.Sprintf("📝 missing type: %s", label))
			}
		}
	}

	// 3. Skill SKILL.md files: must have frontmatter with name, description
	for _, dir := range skillDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			skillMd := filepath.Join(dir, e.Name(), "SKILL.md")
			data, err := os.ReadFile(skillMd)
			if err != nil {
				continue
			}
			label := fmt.Sprintf("skill/%s/SKILL.md", e.Name())
			fm := parseMdFrontmatter(string(data))
			if fm == nil {
				warnings = append(warnings, fmt.Sprintf("📝 missing frontmatter: %s", label))
				continue
			}
			if fm["name"] == "" {
				warnings = append(warnings, fmt.Sprintf("📝 missing name: %s", label))
			}
			if fm["description"] == "" {
				warnings = append(warnings, fmt.Sprintf("📝 missing description: %s (invisible in skill index)", label))
			}
		}
	}

	// 4. CLAUDE.md files in projects: should have substantive content and structure
	// Skip third-party upstream repos we don't own/control — their CLAUDE.md format is not our concern.
	thirdPartyMarkers := []string{
		"/zilliz/milvus/",      // Milvus upstream
		"/goodvision/billing/", // GV upstream
	}
	// `~/code/*` holds cloned upstream tools (gemini-cli, etc.). Match by
	// home-anchored prefix instead of a bare "/code/" substring — the latter
	// also matches paths like "/projects/source-code/" or any directory
	// segment that happens to contain the letters "code".
	codeRoot := filepath.Join(home, "code") + string(os.PathSeparator)
	for _, root := range projectRoots {
	cl:
		for _, f := range findCLAUDEMDs(root, 2) {
			if strings.HasPrefix(f, codeRoot) {
				continue
			}
			for _, m := range thirdPartyMarkers {
				if strings.Contains(f, m) {
					continue cl
				}
			}
			info, err := os.Stat(f)
			if err != nil {
				continue
			}
			shortPath := strings.Replace(f, home, "~", 1)
			if info.Size() == 0 {
				warnings = append(warnings, fmt.Sprintf("📝 empty CLAUDE.md: %s", shortPath))
				continue
			}
			if info.Size() < 20 {
				warnings = append(warnings, fmt.Sprintf("📝 CLAUDE.md too short (%d bytes): %s", info.Size(), shortPath))
				continue
			}
			// Check for at least one markdown heading (structural content)
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			content := string(data)
			hasHeading := false
			for _, line := range strings.Split(content, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					hasHeading = true
					break
				}
			}
			if !hasHeading {
				warnings = append(warnings, fmt.Sprintf("📝 CLAUDE.md has no markdown headings: %s", shortPath))
			}
		}
	}

	// 5. Reverse index: topic files that exist but are not indexed in MEMORY.md.
	// Aggregate into a single warning (with up to 5 example filenames) instead of one per file —
	// previously this produced ~24 individual lines that drowned the rest of the safety report.
	memoryMdContent, memErr := os.ReadFile(filepath.Join(workspace, "MEMORY.md"))
	if memErr == nil {
		memIdx := string(memoryMdContent)
		var unindexed []string
		if entries, err := os.ReadDir(filepath.Join(workspace, "memory", "topics")); err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
					continue
				}
				relPath := "memory/topics/" + e.Name()
				if !strings.Contains(memIdx, relPath) {
					unindexed = append(unindexed, e.Name())
				}
			}
		}
		if len(unindexed) > 0 {
			preview := unindexed
			if len(preview) > 5 {
				preview = preview[:5]
			}
			suffix := ""
			if len(unindexed) > 5 {
				suffix = fmt.Sprintf(" (+%d more)", len(unindexed)-5)
			}
			warnings = append(warnings, fmt.Sprintf("📝 %d topic(s) not indexed in MEMORY.md: %s%s", len(unindexed), strings.Join(preview, ", "), suffix))
		}
	}

	// 6. Soul fragment files: must have proper frontmatter (id, title, modes).
	// validateSoulFragments() is defined in prompt_routing.go.
	warnings = append(warnings, validateSoulFragments()...)

	return warnings
}

// handleLint runs md format validation and prints results
func handleLint() {
	warnings := validateMdFormats()
	if len(warnings) == 0 {
		fmt.Println("✅ all md files properly formatted")
		return
	}
	fmt.Printf("found %d issues:\n\n", len(warnings))
	for _, w := range warnings {
		fmt.Printf("  %s\n", w)
	}
	os.Exit(1)
}
