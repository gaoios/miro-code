package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kiyor/soul-cli/pkg/provider/openai"
)

// serviceEntry defines a monitored service
type serviceEntry struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// loadServices loads the service list from <appDir>/services.json;
// returns the default list if the file does not exist.
func loadServices() []serviceEntry {
	// read from services.json; return empty list if not found
	cfgPath := filepath.Join(appDir, "services.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil
	}
	var svcs []serviceEntry
	if err := json.Unmarshal(data, &svcs); err != nil || len(svcs) == 0 {
		return nil
	}
	return svcs
}

// handleStatus performs a quick local health check without starting claude
func handleStatus() {
	fmt.Printf("%s %s (%s) built %s\n\n", appName, buildVersion, buildCommit, buildDate)

	// 1. Service health (parallel checks)
	fmt.Println("── Service Health ──")
	type svcResult struct {
		name   string
		status int
		err    error
		ms     int64
	}
	results := make([]svcResult, len(monitoredServices))
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 3 * time.Second}
	for i, svc := range monitoredServices {
		wg.Add(1)
		go func(idx int, name, url string) {
			defer wg.Done()
			start := time.Now()
			resp, err := client.Get(url)
			elapsed := time.Since(start).Milliseconds()
			if err != nil {
				results[idx] = svcResult{name: name, err: err, ms: elapsed}
				return
			}
			resp.Body.Close()
			results[idx] = svcResult{name: name, status: resp.StatusCode, ms: elapsed}
		}(i, svc.Name, svc.URL)
	}
	wg.Wait()
	for _, r := range results {
		if r.err != nil {
			fmt.Printf("  ❌ %-12s %dms  %s\n", r.name, r.ms, r.err.Error())
		} else if r.status < 400 {
			fmt.Printf("  ✅ %-12s %d  %dms\n", r.name, r.status, r.ms)
		} else {
			fmt.Printf("  ⚠️  %-12s %d  %dms\n", r.name, r.status, r.ms)
		}
	}

	// 2. Prompt size
	fmt.Println("\n── Prompt ──")
	// temporarily set sessionDir/promptOut for buildPrompt, restore immediately after
	origSessionDir, origPromptOut := sessionDir, promptOut
	sessionDir = os.TempDir()
	promptOut = filepath.Join(sessionDir, appName+"-status-prompt.md")
	result := buildPrompt()
	sessionDir, promptOut = origSessionDir, origPromptOut
	tokens := estimateTokens(result.content)
	fmt.Printf("  size: %d chars, ~%dk tokens (limit %dk)\n", len(result.content), tokens/1000, promptTokenLimit/1000)

	// individual file sizes
	for _, name := range []string{"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "TOOLS.md", "MEMORY.md"} {
		if info, err := os.Stat(filepath.Join(workspace, name)); err == nil {
			t := estimateTokens(string(mustRead(filepath.Join(workspace, name))))
			fmt.Printf("  %-14s %5d bytes  ~%dk tokens\n", name, info.Size(), t/1000)
		}
	}
	today := time.Now().Format("2006-01-02")
	todayNote := filepath.Join(workspace, "memory", today+".md")
	if info, err := os.Stat(todayNote); err == nil {
		t := estimateTokens(string(mustRead(todayNote)))
		fmt.Printf("  %-14s %5d bytes  ~%dk tokens\n", "today note", info.Size(), t/1000)
	}

	// 3. Session DB
	fmt.Println("\n── Session DB ──")
	db, err := openDB()
	if err == nil {
		defer db.Close()
		var total, withSummary int
		db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&total)
		db.QueryRow("SELECT COUNT(*) FROM sessions WHERE summary != ''").Scan(&withSummary)
		fmt.Printf("  total: %d sessions, %d summarized\n", total, withSummary)

		// pending
		sessions := recentSessions(20)
		states := checkSessions(db, sessions)
		pending := 0
		for _, s := range states {
			if s.changed || s.summary == "" {
				pending++
			}
		}
		if pending > 0 {
			fmt.Printf("  pending: %d\n", pending)
		}
	}

	// 4. Versions
	fmt.Println("\n── Version History ──")
	hasVersions := false
	for i := 1; i <= maxVersions; i++ {
		meta, err := loadMeta(i)
		if err != nil {
			continue
		}
		hasVersions = true
		info, _ := os.Stat(versionBinPath(i))
		sizeStr := "?"
		if info != nil {
			sizeStr = fmt.Sprintf("%.1fMB", float64(info.Size())/1024/1024)
		}
		fmt.Printf("  .%d  %s  %s  %s\n", i, meta.Timestamp.Format("01-02 15:04"), sizeStr, meta.Hash[:12])
	}
	if !hasVersions {
		fmt.Println("  (no saved versions)")
	}

	// 5. Recent cron/heartbeat
	fmt.Println("\n── Latest Runs ──")
	printLastRuns()

	// 6. Metrics trends
	fmt.Println("\n── Metrics ──")
	printMetricsSummary()

	// 7. Temp dirs
	fmt.Println("\n── Temp Files ──")
	tmpDirs := findAppTmpDirs()
	if len(tmpDirs) == 0 {
		fmt.Println("  (none)")
	} else {
		var totalSize int64
		for _, d := range tmpDirs {
			totalSize += dirSize(d)
		}
		fmt.Printf("  %d session dirs, total %s\n", len(tmpDirs), humanSize(totalSize))
	}
}

// handleDoctor runs deep diagnostics, more thorough than status
func handleDoctor() {
	fmt.Printf("%s %s (%s) built %s\n\n", appName, buildVersion, buildCommit, buildDate)
	issues := 0
	warn := func(format string, a ...interface{}) {
		issues++
		fmt.Printf("  ⚠️  "+format+"\n", a...)
	}
	ok := func(format string, a ...interface{}) {
		fmt.Printf("  ✅ "+format+"\n", a...)
	}

	// 1. Claude binary
	fmt.Println("── Claude Binary ──")
	if info, err := os.Stat(claudeBin); err != nil {
		warn("claude not found: %s", claudeBin)
	} else {
		// get version
		out, err := exec.Command(claudeBin, "--version").CombinedOutput()
		if err != nil {
			warn("claude --version failed: %v", err)
		} else {
			ok("claude %s (%.1f MB)", strings.TrimSpace(string(out)), float64(info.Size())/1024/1024)
		}
	}

	// 2. Model endpoints
	fmt.Println("\n── Model Endpoints ──")
	endpoint := getModelEndpoint()
	if endpoint == "" {
		warn("cannot read model endpoint config")
	} else {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(endpoint)
		if err != nil {
			warn("model endpoint unreachable: %s", err)
		} else {
			resp.Body.Close()
			if resp.StatusCode >= 500 {
				warn("model endpoint error: %s → %d", endpoint, resp.StatusCode)
			} else {
				ok("model endpoint ok: %s → %d", endpoint, resp.StatusCode)
			}
		}
	}

	// 4. Disk space
	fmt.Println("\n── Disk Space ──")
	var stat syscall.Statfs_t
	if err := syscall.Statfs(workspace, &stat); err == nil {
		freeGB := float64(stat.Bavail*uint64(stat.Bsize)) / (1024 * 1024 * 1024)
		totalGB := float64(stat.Blocks*uint64(stat.Bsize)) / (1024 * 1024 * 1024)
		usedPct := (1 - float64(stat.Bavail)/float64(stat.Blocks)) * 100
		if freeGB < 1.0 {
			warn("disk space low: %.1f GB / %.0f GB (%.0f%% used)", freeGB, totalGB, usedPct)
		} else {
			ok("%.1f GB available / %.0f GB (%.0f%% used)", freeGB, totalGB, usedPct)
		}
	}

	// 5. Stale lock detection
	fmt.Println("\n── Lock Status ──")
	lockDir := lockfile + ".d"
	if pid, hasPid := readLockPid(lockDir); hasPid {
		// check if the process still exists
		proc, _ := os.FindProcess(pid)
		if proc != nil && proc.Signal(syscall.Signal(0)) == nil {
			ok("lock held by PID %d (active)", pid)
		} else {
			warn("stale lock detected: PID %d no longer exists, suggest removing %s", pid, lockDir)
		}
	} else if _, err := os.Stat(lockDir); err == nil {
		warn("empty lock dir exists: %s (no PID file, possibly residual)", lockDir)
	} else {
		ok("no lock")
	}

	// 6. DB integrity
	fmt.Println("\n── Session DB ──")
	db, err := openDB()
	if err != nil {
		warn("failed to open DB: %v", err)
	} else {
		defer db.Close()
		var result string
		if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
			warn("DB integrity_check failed: %v", err)
		} else if result != "ok" {
			warn("DB corrupted: %s", result)
		} else {
			var total, withSummary int
			db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&total)
			db.QueryRow("SELECT COUNT(*) FROM sessions WHERE summary != ''").Scan(&withSummary)

			// check for orphaned records (files deleted)
			rows, _ := db.Query("SELECT path FROM sessions")
			orphaned := 0
			if rows != nil {
				for rows.Next() {
					var p string
					rows.Scan(&p)
					if _, err := os.Stat(p); os.IsNotExist(err) {
						orphaned++
					}
				}
				rows.Close()
			}

			ok("DB healthy (sessions=%d, summarized=%d)", total, withSummary)
			if orphaned > 0 {
				warn("DB has %d orphaned records (files deleted), run `%s db gc` to clean up", orphaned, appName)
			}
		}
	}

	// 7. Metrics anomaly detection
	fmt.Println("\n── Metrics Health ──")
	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	if data, err := os.ReadFile(metricsPath); err == nil {
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")

		// compute success rate for each mode from the last 20 entries
		type modeCount struct{ total, fail int }
		recent := make(map[string]*modeCount)
		start := 0
		if len(lines) > 20 {
			start = len(lines) - 20
		}
		for _, line := range lines[start:] {
			var rec struct {
				Mode     string `json:"mode"`
				ExitCode int    `json:"exit_code"`
			}
			if json.Unmarshal([]byte(line), &rec) != nil || rec.Mode == "" {
				continue
			}
			c, found := recent[rec.Mode]
			if !found {
				c = &modeCount{}
				recent[rec.Mode] = c
			}
			c.total++
			if rec.ExitCode != 0 {
				c.fail++
			}
		}

		for mode, c := range recent {
			failRate := float64(c.fail) / float64(c.total) * 100
			if failRate > 50 {
				warn("%s: %.0f%% failure rate (%d failures in last %d runs)", mode, failRate, c.fail, c.total)
			} else {
				ok("%s: %.0f%% success rate (%d/%d)", mode, 100-failRate, c.total-c.fail, c.total)
			}
		}
		ok("metrics.jsonl: %d records", len(lines))
	} else {
		warn("no metrics.jsonl")
	}

	// 8. Soul file integrity
	fmt.Println("\n── Soul Files ──")
	soulFiles := []string{"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "TOOLS.md", "MEMORY.md", "HEARTBEAT.md"}
	for _, f := range soulFiles {
		path := filepath.Join(workspace, f)
		info, err := os.Lstat(path)
		if err != nil {
			warn("%s missing", f)
		} else if info.Mode()&os.ModeSymlink != 0 {
			warn("%s is symlink (possibly tampered)", f)
		} else if info.Size() == 0 {
			warn("%s empty file", f)
		} else {
			// no output for healthy files, keep it clean
		}
	}
	allSoulOK := true
	for _, f := range soulFiles {
		path := filepath.Join(workspace, f)
		info, err := os.Lstat(path)
		if err != nil || info.Size() == 0 || info.Mode()&os.ModeSymlink != 0 {
			allSoulOK = false
			break
		}
	}
	if allSoulOK {
		ok("all soul files intact (%d files)", len(soulFiles))
	}

	// 9. Markdown format validation
	fmt.Println("\n── Markdown Formats ──")
	mdWarnings := validateMdFormats()
	if len(mdWarnings) == 0 {
		ok("all md files properly formatted")
	} else {
		for _, w := range mdWarnings {
			warn("%s", w)
		}
	}

	// 10. Temp files
	fmt.Println("\n── Temp Files ──")
	tmpDirs := findAppTmpDirs()
	if len(tmpDirs) == 0 {
		ok("no temp dirs")
	} else {
		var totalSize int64
		old := 0
		cutoff := time.Now().Add(-6 * time.Hour)
		for _, d := range tmpDirs {
			totalSize += dirSize(d)
			if info, err := os.Stat(d); err == nil && info.ModTime().Before(cutoff) {
				old++
			}
		}
		if old > 0 {
			warn("%d temp dirs (%s), %d older than 6 hours, run `%s clean`", len(tmpDirs), humanSize(totalSize), old, appName)
		} else {
			ok("%d temp dirs (%s)", len(tmpDirs), humanSize(totalSize))
		}
	}

	// Summary
	fmt.Println()
	if issues == 0 {
		fmt.Println("🩺 diagnostics complete: all clear")
	} else {
		fmt.Printf("🩺 diagnostics complete: found %d issues\n", issues)
	}
}

// handleDoctorCron audits the crontab entries for weiran.
// Checks: binary path matches installed binary, schedule sanity, log health,
// evolve-probe readiness, feedback v2 coverage.
func handleDoctorCron() {
	fmt.Printf("%s doctor cron — crontab audit\n\n", appName)
	issues := 0
	warn := func(format string, a ...interface{}) {
		issues++
		fmt.Printf("  ⚠️  "+format+"\n", a...)
	}
	ok := func(format string, a ...interface{}) {
		fmt.Printf("  ✅ "+format+"\n", a...)
	}
	info := func(format string, a ...interface{}) {
		fmt.Printf("  ℹ️  "+format+"\n", a...)
	}

	installedBin, _ := filepath.Abs(os.Args[0])
	// Also check the make install target
	localBin := filepath.Join(home, ".local", "bin", appName)

	// 1. Parse crontab
	fmt.Println("── Crontab Entries ──")
	cronOut, err := exec.Command("crontab", "-l").Output()
	if err != nil {
		warn("crontab -l failed: %v", err)
		fmt.Printf("\n🩺 audit complete: %d issues\n", issues)
		return
	}

	type cronEntry struct {
		schedule string
		binary   string
		args     string
		logFile  string
		raw      string
	}

	var entries []cronEntry
	for _, line := range strings.Split(string(cronOut), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Check if this line involves our app
		if !strings.Contains(line, appName) && !strings.Contains(line, "soul") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 6 {
			continue
		}
		sched := strings.Join(parts[:5], " ")
		cmd := strings.Join(parts[5:], " ")

		// Extract binary path (first token of command)
		cmdParts := strings.Fields(cmd)
		binary := cmdParts[0]

		// Extract args (up to >>)
		var args []string
		var logFile string
		for i, p := range cmdParts[1:] {
			if p == ">>" {
				if i+2 < len(cmdParts) {
					logFile = cmdParts[i+2]
				}
				break
			}
			args = append(args, p)
		}

		entries = append(entries, cronEntry{
			schedule: sched,
			binary:   binary,
			args:     strings.Join(args, " "),
			logFile:  logFile,
			raw:      line,
		})
	}

	if len(entries) == 0 {
		warn("no %s entries found in crontab", appName)
		fmt.Printf("\n🩺 audit complete: %d issues\n", issues)
		return
	}

	for _, e := range entries {
		fmt.Printf("\n  %s  %s %s\n", e.schedule, filepath.Base(e.binary), e.args)
	}
	fmt.Println()

	// 2. Binary path check
	fmt.Println("── Binary Path ──")
	for _, e := range entries {
		// Check if the crontab binary matches the installed one
		cronBinReal, _ := filepath.EvalSymlinks(e.binary)
		localBinReal, _ := filepath.EvalSymlinks(localBin)
		installedReal, _ := filepath.EvalSymlinks(installedBin)

		if cronBinReal != localBinReal && cronBinReal != installedReal {
			// Check if crontab binary is older
			cronInfo, err1 := os.Stat(e.binary)
			localInfo, err2 := os.Stat(localBin)
			if err1 == nil && err2 == nil {
				if cronInfo.ModTime().Before(localInfo.ModTime()) {
					warn("%s uses OLD binary: %s (mod %s)\n       installed: %s (mod %s)\n       fix: crontab -e → replace %s with %s",
						e.args, e.binary, cronInfo.ModTime().Format("2006-01-02 15:04"),
						localBin, localInfo.ModTime().Format("2006-01-02 15:04"),
						e.binary, localBin)
				} else {
					info("%s: binary differs from installed (%s vs %s)", e.args, e.binary, localBin)
				}
			} else {
				warn("%s: binary path %s — cannot stat: %v", e.args, e.binary, err1)
			}
		} else {
			ok("%s: binary path matches installed", e.args)
		}
	}

	// 3. Schedule sanity
	fmt.Println("\n── Schedule Sanity ──")
	hasHeartbeat, hasCron, hasEvolve := false, false, false
	for _, e := range entries {
		switch {
		case strings.Contains(e.args, "--heartbeat"):
			hasHeartbeat = true
			// Should be every 10-30 minutes
			if strings.HasPrefix(e.schedule, "*/") {
				parts := strings.Split(e.schedule, " ")
				if interval := parts[0]; interval == "*/15" || interval == "*/10" || interval == "*/20" || interval == "*/30" {
					ok("heartbeat: %s (good interval)", e.schedule)
				} else {
					info("heartbeat: %s (unusual interval, expected */10-30)", e.schedule)
				}
			}
		case strings.Contains(e.args, "--cron"):
			hasCron = true
			ok("cron: %s", e.schedule)
		case strings.Contains(e.args, "--evolve"):
			hasEvolve = true
			ok("evolve: %s", e.schedule)
		}
	}
	if !hasHeartbeat {
		warn("no --heartbeat entry in crontab")
	}
	if !hasCron {
		warn("no --cron entry in crontab")
	}
	if !hasEvolve {
		warn("no --evolve entry in crontab")
	}

	// 4. Log file health
	fmt.Println("\n── Log Files ──")
	for _, e := range entries {
		if e.logFile == "" {
			info("%s: no log file configured", e.args)
			continue
		}
		logInfo, err := os.Stat(e.logFile)
		if err != nil {
			info("%s: log not found yet (%s)", e.args, e.logFile)
		} else {
			age := time.Since(logInfo.ModTime())
			sizeKB := logInfo.Size() / 1024
			if age > 48*time.Hour && strings.Contains(e.args, "--heartbeat") {
				warn("%s: log stale (last write %s ago) — cron may not be running", e.args, age.Round(time.Hour))
			} else if sizeKB > 10240 {
				warn("%s: log large (%d KB), consider rotation", e.args, sizeKB)
			} else {
				ok("%s: log ok (%d KB, last write %s ago)", e.args, sizeKB, age.Round(time.Minute))
			}
		}
	}

	// 5. Evolve-probe readiness
	fmt.Println("\n── Evolve-Probe Readiness ──")
	feedbacks, err := listActiveFeedbacks("")
	if err != nil {
		warn("cannot list feedbacks: %v", err)
	} else {
		withScenarios := 0
		noScenarios := 0
		for _, fb := range feedbacks {
			if len(fb.TestScenarios) > 0 {
				withScenarios++
			} else {
				noScenarios++
				warn("feedback %q has no test_scenarios", fb.Name)
			}
		}
		if noScenarios == 0 {
			ok("%d active feedbacks, all have test_scenarios", withScenarios)
		} else {
			warn("%d/%d feedbacks missing test_scenarios", noScenarios, withScenarios+noScenarios)
		}

		// Check for never-probed feedbacks
		neverProbed := 0
		for _, fb := range feedbacks {
			if feedbackLastProbed(fb).IsZero() {
				neverProbed++
			}
		}
		if neverProbed > 0 {
			info("%d feedbacks never probed (will be prioritized by --sample)", neverProbed)
		}
	}

	// 6. Evolve template check — does it reference evolve-probe?
	fmt.Println("\n── Evolve Template ──")
	evolveOut := evolveTask()
	if strings.Contains(evolveOut, "evolve-probe") {
		ok("evolve template includes probe phase")
	} else {
		warn("evolve template does NOT reference evolve-probe — Phase 3 missing?")
	}
	if strings.Contains(evolveOut, "Invariant Check") || strings.Contains(evolveOut, "invariants") {
		ok("evolve template includes invariant check phase")
	} else {
		warn("evolve template missing invariant check phase")
	}
	if strings.Contains(evolveOut, "Fact Drift") || strings.Contains(evolveOut, "fact drift") {
		ok("evolve template includes fact drift reconciler")
	} else {
		info("evolve template missing fact drift reconciler (optional)")
	}

	// 7. Metrics — recent run success
	fmt.Println("\n── Recent Run History ──")
	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	if data, err := os.ReadFile(metricsPath); err == nil {
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		modes := map[string]struct{ total, fail int }{}
		var lastRun map[string]time.Time = make(map[string]time.Time)
		for _, line := range lines {
			var rec struct {
				Mode     string `json:"mode"`
				ExitCode int    `json:"exit_code"`
				Ts       string `json:"ts"`
			}
			if json.Unmarshal([]byte(line), &rec) != nil || rec.Mode == "" {
				continue
			}
			// Only count modes we care about
			switch rec.Mode {
			case "heartbeat", "cron", "evolve":
			default:
				continue
			}
			m := modes[rec.Mode]
			m.total++
			if rec.ExitCode != 0 {
				m.fail++
			}
			modes[rec.Mode] = m
			if t, err := time.Parse(time.RFC3339, rec.Ts); err == nil {
				lastRun[rec.Mode] = t
			}
		}

		for _, mode := range []string{"heartbeat", "cron", "evolve"} {
			m, exists := modes[mode]
			if !exists {
				warn("%s: never run (no metrics)", mode)
				continue
			}
			failRate := float64(m.fail) / float64(m.total) * 100
			last := lastRun[mode]
			ago := time.Since(last).Round(time.Hour)
			if failRate > 50 {
				warn("%s: %.0f%% failure rate (%d/%d), last run %s ago", mode, failRate, m.fail, m.total, ago)
			} else {
				ok("%s: %.0f%% success (%d/%d), last run %s ago", mode, 100-failRate, m.total-m.fail, m.total, ago)
			}
		}
	} else {
		warn("no metrics.jsonl — cannot check run history")
	}

	// Summary
	fmt.Println()
	if issues == 0 {
		fmt.Println("🩺 cron audit complete: all clear")
	} else {
		fmt.Printf("🩺 cron audit complete: found %d issues\n", issues)
	}
}

// handleDiff shows soul/memory file changes since last commit
func handleDiff() {
	// files of interest
	patterns := []string{
		"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "TOOLS.md",
		"MEMORY.md", "HEARTBEAT.md", "memory/",
	}

	// git status (only look at files of interest under workspace)
	statusOut, err := exec.Command("git", "-C", workspace, "status", "--porcelain").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "git status failed: %v\n", err)
		return
	}

	if len(statusOut) == 0 {
		fmt.Println("no changes (working tree clean)")
		return
	}

	// filter to show only relevant files
	lines := strings.Split(strings.TrimSpace(string(statusOut)), "\n")
	var relevant []string
	for _, line := range lines {
		if len(line) < 3 {
			continue
		}
		file := strings.TrimSpace(line[2:])
		for _, pat := range patterns {
			if strings.HasPrefix(file, pat) || file == pat {
				relevant = append(relevant, line)
				break
			}
		}
	}

	if len(relevant) == 0 {
		fmt.Println("no soul/memory file changes")
		return
	}

	// show change summary
	fmt.Printf("── Soul/Memory Changes (%d files) ──\n\n", len(relevant))

	statusIcon := map[byte]string{
		'M': "modified", 'A': "added", 'D': "deleted", '?': "untracked", 'R': "renamed",
	}

	for _, line := range relevant {
		code := line[0]
		if code == ' ' {
			code = line[1] // unstaged change
		}
		label := statusIcon[code]
		if label == "" {
			label = string(code)
		}
		file := strings.TrimSpace(line[2:])
		fmt.Printf("  [%s] %s\n", label, file)
	}

	// show diff stat
	fmt.Println()
	var diffArgs []string
	for _, pat := range patterns {
		diffArgs = append(diffArgs, "--", pat)
	}
	args := append([]string{"-C", workspace, "diff", "--stat"}, diffArgs...)
	diffOut, err := exec.Command("git", args...).Output()
	if err == nil && len(diffOut) > 0 {
		fmt.Println("── Diff Stats ──")
		fmt.Print(string(diffOut))
	}

	// if there are actual content changes, show a brief diff (max 50 lines)
	args = append([]string{"-C", workspace, "diff", "--unified=2"}, diffArgs...)
	fullDiff, err := exec.Command("git", args...).Output()
	if err == nil && len(fullDiff) > 0 {
		lines := strings.Split(string(fullDiff), "\n")
		fmt.Println("\n── Diff Preview ──")
		limit := 50
		if len(lines) < limit {
			limit = len(lines)
		}
		for _, line := range lines[:limit] {
			// simple colorization
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				fmt.Printf("\033[32m%s\033[0m\n", line)
			} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				fmt.Printf("\033[31m%s\033[0m\n", line)
			} else if strings.HasPrefix(line, "@@") {
				fmt.Printf("\033[36m%s\033[0m\n", line)
			} else if strings.HasPrefix(line, "diff ") {
				fmt.Printf("\033[1m%s\033[0m\n", line)
			} else {
				fmt.Println(line)
			}
		}
		if len(lines) > limit {
			fmt.Printf("\n... %d more lines (run `git -C %s diff` to see full diff)\n", len(lines)-limit, workspace)
		}
	}
}

// handleConfig shows the current configuration
func handleConfig() {
	fmt.Printf("%s %s (%s) built %s\n\n", appName, buildVersion, buildCommit, buildDate)
	fmt.Println("── Paths ──")
	fmt.Printf("  workspace:    %s\n", workspace)
	fmt.Printf("  claude:       %s\n", claudeBin)
	fmt.Printf("  app bin:      %s\n", appBin)
	fmt.Printf("  app data:     %s\n", appDir)
	fmt.Printf("  source:       %s\n", srcDir)
	fmt.Printf("  versions:     %s\n", versionsDir)
	fmt.Printf("  db:           %s\n", dbPath)
	fmt.Printf("  hooks:        %s\n", hooksDir)
	fmt.Printf("  lockfile:     %s\n", lockfile)

	fmt.Println("\n── Agent ──")
	fmt.Printf("  name:         %s\n", agentName)
	fmt.Printf("  tg chat:      %s\n", tgChatID)
	maskedJira := "(not set)"
	if jiraToken != "" {
		if len(jiraToken) > 8 {
			maskedJira = jiraToken[:4] + "…" + jiraToken[len(jiraToken)-4:]
		} else {
			maskedJira = "****"
		}
	}
	fmt.Printf("  jira token:   %s\n", maskedJira)

	fmt.Println("\n── Skill Dirs ──")
	for _, d := range skillDirs {
		exists := "✅"
		if _, err := os.Stat(d); err != nil {
			exists = "❌"
		}
		fmt.Printf("  %s %s\n", exists, d)
	}

	fmt.Println("\n── Project Dirs ──")
	for _, d := range projectRoots {
		exists := "✅"
		if _, err := os.Stat(d); err != nil {
			exists = "❌"
		}
		fmt.Printf("  %s %s\n", exists, d)
	}

}

// handleModels lists every model available via --model, grouped by provider.
// Reads from config.json (providers section) and also documents native
// Anthropic aliases that Claude Code ships with.
func handleModels() {
	providers, source := loadAllProviders()

	fmt.Printf("%s %s — available models\n", appName, buildVersion)
	if source != "" {
		fmt.Printf("source: %s\n\n", source)
	} else {
		fmt.Println("source: (no config.json with providers section found)")
		fmt.Println()
	}

	// Native Anthropic aliases — handled by Claude Code directly, not by
	// our provider injection. These work without the `provider/` prefix.
	fmt.Println("── Native (Anthropic, upstream default) ──")
	nativeAliases := [][2]string{
		{"opus", "claude-opus-4-7"},
		{"opus[1m]", "claude-opus-4-7[1m]"},
		{"opus-4-6", "claude-opus-4-6"},
		{"opus-4-6[1m]", "claude-opus-4-6[1m]"},
		{"opus-4-7", "claude-opus-4-7"},
		{"opus-4-7[1m]", "claude-opus-4-7[1m]"},
		{"sonnet", "claude-sonnet-4-6"},
		{"sonnet[1m]", "claude-sonnet-4-6[1m]"},
		{"haiku", "claude-haiku-4-5-20251001"},
	}
	for _, row := range nativeAliases {
		fmt.Printf("  %-14s → %s\n", row[0], row[1])
	}
	fmt.Printf("  usage: %s --model opus   (no provider prefix)\n", appName)
	fmt.Println()

	// Custom providers from config.json
	if len(providers) == 0 {
		fmt.Println("── Custom providers ──")
		fmt.Println("  (none configured — add entries to config.json `providers`)")
	} else {
		// Sort provider names for stable output
		names := make([]string, 0, len(providers))
		for n := range providers {
			names = append(names, n)
		}
		sort.Strings(names)

		for _, name := range names {
			prov := providers[name]
			fmt.Printf("── %s ──\n", name)
			if prov.Type == "openai" {
				fmt.Printf("  type: openai proxy → %s\n", func() string {
					if prov.ChatURL != "" {
						return prov.ChatURL
					}
					return openai.Endpoint
				}())
			} else {
				fmt.Printf("  endpoint: %s\n", prov.BaseURL)
			}
			if prov.AuthEnv != "" {
				fmt.Printf("  auth env: %s\n", prov.AuthEnv)
			}
			if len(prov.Models) == 0 {
				fmt.Println("  models:   (none listed — any model name will be passed through without validation)")
			} else {
				fmt.Println("  models:")
				for _, m := range prov.Models {
					fmt.Printf("    %s/%s\n", name, m)
				}
			}
			fmt.Println()
		}
	}

	if defaultModel != "" {
		fmt.Printf("Default model (cron/heartbeat/evolve): %s\n", defaultModel)
		if len(defaultModelFallbacks) > 0 {
			fmt.Printf("  Fallbacks: %s\n", strings.Join(defaultModelFallbacks, " → "))
		}
	}
	fmt.Printf("Usage: %s --model <name>   (e.g. `%s --model minimax/MiniMax-M2.7-highspeed`)\n", appName, appName)
}

// handleClean cleans old session temp dirs in /tmp
func handleClean() {
	tmpDirs := findAppTmpDirs()
	if len(tmpDirs) == 0 {
		fmt.Println("nothing to clean")
		return
	}

	// keep dirs from last hour
	cutoff := time.Now().Add(-1 * time.Hour)
	cleaned := 0
	var freedBytes int64
	for _, d := range tmpDirs {
		info, err := os.Stat(d)
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		size := dirSize(d)
		if err := os.RemoveAll(d); err != nil {
			fmt.Fprintf(os.Stderr, "  delete failed: %s — %v\n", filepath.Base(d), err)
			continue
		}
		fmt.Printf("  deleted: %s (%s)\n", filepath.Base(d), humanSize(size))
		cleaned++
		freedBytes += size
	}
	if cleaned == 0 {
		fmt.Println("nothing to clean (all within 1 hour)")
	} else {
		fmt.Printf("cleanup done: %d dirs, freed %s\n", cleaned, humanSize(freedBytes))
	}
}

func findAppTmpDirs() []string {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), agentName+"-") {
			dirs = append(dirs, filepath.Join(os.TempDir(), e.Name()))
		}
	}
	return dirs
}

func dirSize(path string) int64 {
	var total int64
	visited := make(map[deviceInode]bool)
	filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info == nil {
			return nil
		}
		// skip symlinks to prevent infinite recursion from cycles
		if info.Mode()&os.ModeSymlink != 0 {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// cycle prevention: check inode
		if di, ok := getDeviceInode(p); ok {
			if visited[di] {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			visited[di] = true
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func mustRead(path string) []byte {
	data, _ := os.ReadFile(path)
	return data
}

// handleLog views daily notes, defaults to today, supports day offset
func handleLog(args []string) {
	offset := 0
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil && n >= 0 {
			offset = n
		}
	}
	day := time.Now().AddDate(0, 0, -offset).Format("2006-01-02")
	notePath := filepath.Join(workspace, "memory", day+".md")

	data, err := os.ReadFile(notePath)
	if err != nil {
		fmt.Printf("no daily notes for %s\n", day)
		// list recent available daily notes
		entries, _ := os.ReadDir(filepath.Join(workspace, "memory"))
		if len(entries) > 0 {
			fmt.Println("\navailable daily notes:")
			count := 0
			for i := len(entries) - 1; i >= 0 && count < 7; i-- {
				e := entries[i]
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") && !strings.Contains(e.Name(), "topics") {
					info, _ := e.Info()
					if info != nil {
						fmt.Printf("  %s  %s\n", e.Name(), humanSize(info.Size()))
						count++
					}
				}
			}
		}
		return
	}
	fmt.Printf("# Daily Notes: %s (%s)\n\n", day, humanSize(int64(len(data))))
	fmt.Print(string(data))
}

// printLastRuns finds the last run time per mode from metrics.jsonl
func printLastRuns() {
	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	data, err := os.ReadFile(metricsPath)
	if err != nil {
		fmt.Println("  (no metrics data)")
		return
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	// scan from tail, find last entry per mode
	lastRun := make(map[string]struct {
		ts       string
		exitCode int
	})
	for i := len(lines) - 1; i >= 0; i-- {
		var rec struct {
			Timestamp string `json:"ts"`
			Mode      string `json:"mode"`
			ExitCode  int    `json:"exit_code"`
		}
		if json.Unmarshal([]byte(lines[i]), &rec) != nil || rec.Mode == "" {
			continue
		}
		if _, seen := lastRun[rec.Mode]; !seen {
			lastRun[rec.Mode] = struct {
				ts       string
				exitCode int
			}{rec.Timestamp, rec.ExitCode}
		}
	}

	if len(lastRun) == 0 {
		fmt.Println("  (no records)")
		return
	}

	for mode, info := range lastRun {
		t, err := time.Parse(time.RFC3339, info.ts)
		ago := "?"
		if err == nil {
			d := time.Since(t)
			switch {
			case d < time.Hour:
				ago = fmt.Sprintf("%dm ago", int(d.Minutes()))
			case d < 24*time.Hour:
				ago = fmt.Sprintf("%dh ago", int(d.Hours()))
			default:
				ago = fmt.Sprintf("%dd ago", int(d.Hours()/24))
			}
		}
		status := "✅"
		if info.exitCode != 0 {
			status = fmt.Sprintf("❌ (exit %d)", info.exitCode)
		}
		fmt.Printf("  %-12s  %s  %s  %s\n", mode, info.ts[:19], ago, status)
	}
}

// printMetricsSummary shows recent metrics trends
func printMetricsSummary() {
	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	data, err := os.ReadFile(metricsPath)
	if err != nil {
		fmt.Println("  (no metrics data)")
		return
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	type modeStats struct {
		total    int
		success  int
		totalDur float64
	}
	stats := make(map[string]*modeStats)

	// only look at the last 50 entries
	start := 0
	if len(lines) > 50 {
		start = len(lines) - 50
	}
	for _, line := range lines[start:] {
		var rec struct {
			Mode     string  `json:"mode"`
			ExitCode int     `json:"exit_code"`
			Duration float64 `json:"duration_s"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Mode == "" {
			continue
		}
		s, ok := stats[rec.Mode]
		if !ok {
			s = &modeStats{}
			stats[rec.Mode] = s
		}
		s.total++
		if rec.ExitCode == 0 {
			s.success++
		}
		s.totalDur += rec.Duration
	}

	if len(stats) == 0 {
		fmt.Println("  (no valid records)")
		return
	}

	fmt.Printf("  %-12s  %5s  %7s  %8s\n", "Mode", "Count", "Success", "Avg Dur")
	for mode, s := range stats {
		pct := 0
		if s.total > 0 {
			pct = s.success * 100 / s.total
		}
		avgDur := 0.0
		if s.total > 0 {
			avgDur = s.totalDur / float64(s.total)
		}
		fmt.Printf("  %-12s  %5d  %5d%%  %7.0fs\n", mode, s.total, pct, avgDur)
	}
	fmt.Printf("  (last %d records)\n", len(lines)-start)
}
