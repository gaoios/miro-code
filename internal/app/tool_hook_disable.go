package app

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ── Temporary, time-bounded disable for tool-hook rules ──
//
// Design: a rule's DisableUntil + DisableReason are persisted in tool-hooks.yaml
// (so the state is inspectable via `git diff`, visible in `weiran tool-hook
// rules`, and shared across all hook invocations without coordination). The
// model grants itself a short exemption with:
//
//	weiran tool-hook disable <rule-id> --until +30m --reason "…"
//
// `--until` is mandatory — that's the whole point: the window must be bounded
// so an accidental disable self-heals. Cap is 2h per disable; re-apply to
// extend. Anything beyond is rejected so the model is forced to re-evaluate
// instead of disabling rules "forever".
//
// Writing back to YAML uses yaml.v3's Node API (surgical edits preserving
// comments + formatting). Marshaling the whole struct would destroy all the
// hand-written comments in tool-hooks.yaml.

// maxTempDisable caps a single disable window. Main rule's core design: the
// model must be forced to re-evaluate regularly rather than disable a hook
// indefinitely. Re-apply to extend when genuinely needed.
const maxTempDisable = 2 * time.Hour

// isTempDisabled returns true if the rule has a non-expired DisableUntil.
// An expired DisableUntil is treated as no disable (self-healing — no cleanup
// required to restore the rule).
func isTempDisabled(r *ToolHookRule) bool {
	if r.DisableUntil == nil {
		return false
	}
	return time.Now().Before(*r.DisableUntil)
}

// tempDisableReason formats the SkipReason for audit rows. Shape:
//
//	temp_disabled until=2026-04-22T23:45:00-07:00 reason="…"
//
// Kept parseable (KV) so `tool-hook log --grep temp_disabled` is useful.
func tempDisableReason(r *ToolHookRule) string {
	until := ""
	if r.DisableUntil != nil {
		until = r.DisableUntil.Format(time.RFC3339)
	}
	return fmt.Sprintf("temp_disabled until=%s reason=%q", until, r.DisableReason)
}

// parseUntilSpec accepts either a relative duration prefixed with "+" (e.g.
// "+30m", "+1h30m") or an RFC3339 absolute timestamp. Negative durations and
// durations exceeding maxTempDisable are rejected with actionable errors.
func parseUntilSpec(spec string, now time.Time) (time.Time, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return time.Time{}, fmt.Errorf("--until is required (e.g. +30m, +2h, or RFC3339)")
	}
	var target time.Time
	if strings.HasPrefix(spec, "+") {
		d, err := time.ParseDuration(spec[1:])
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid duration %q (examples: +30m, +1h, +2h): %v", spec, err)
		}
		target = now.Add(d)
	} else if strings.HasPrefix(spec, "-") {
		return time.Time{}, fmt.Errorf("negative durations not allowed (%q)", spec)
	} else {
		t, err := time.Parse(time.RFC3339, spec)
		if err != nil {
			// Helpful hint: most common mistake is to omit the leading "+"
			if _, dErr := time.ParseDuration(spec); dErr == nil {
				return time.Time{}, fmt.Errorf("%q looks like a duration — prefix with + (e.g. +%s)", spec, spec)
			}
			return time.Time{}, fmt.Errorf("invalid --until %q (must be +duration or RFC3339): %v", spec, err)
		}
		target = t
	}
	delta := target.Sub(now)
	if delta <= 0 {
		return time.Time{}, fmt.Errorf("--until must be in the future (got %s ago)", (-delta).Round(time.Second))
	}
	if delta > maxTempDisable {
		return time.Time{}, fmt.Errorf("--until exceeds %s cap (got %s); re-apply later to extend",
			maxTempDisable, delta.Round(time.Second))
	}
	return target, nil
}

// humanizeDelta renders a duration for human output: "in 29m", "expired 3m ago".
func humanizeDelta(d time.Duration) string {
	if d >= 0 {
		return "in " + d.Round(time.Second).String()
	}
	return "expired " + (-d).Round(time.Second).String() + " ago"
}

// ── YAML Node-based patch ──
//
// tool-hooks.yaml contains hand-written comments for every rule (explaining
// what it guards and why it matters). Whole-file Marshal would destroy them,
// so we surgically patch the target rule node and serialize back through the
// Node API which preserves comments + original style.

// mappingGet returns the value node for a given key in a MappingNode, or nil.
// MappingNode stores keys and values as alternating entries in Content: at
// even indices are the key scalars, at odd indices are the value nodes.
func mappingGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// mappingString returns the scalar string value for a key, or "".
func mappingString(m *yaml.Node, key string) string {
	v := mappingGet(m, key)
	if v == nil || v.Kind != yaml.ScalarNode {
		return ""
	}
	return v.Value
}

// mappingSet inserts or updates a (key, scalar value) pair in a MappingNode.
// If the key already exists, the value scalar is updated in place (preserving
// any head/line comments attached to that key). Otherwise the pair is appended.
func mappingSet(m *yaml.Node, key, value string) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1].Kind = yaml.ScalarNode
			m.Content[i+1].Tag = "!!str"
			m.Content[i+1].Value = value
			m.Content[i+1].Style = 0
			return
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	m.Content = append(m.Content, keyNode, valNode)
}

// mappingDelete removes a (key, value) pair from a MappingNode. Returns true
// if anything was removed. Safe to call even when the key is absent.
func mappingDelete(m *yaml.Node, key string) bool {
	if m == nil || m.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return true
		}
	}
	return false
}

// findRulesSeq walks a parsed YAML doc (root) down to the `rules:` sequence.
// Returns nil if the document is malformed.
func findRulesSeq(root *yaml.Node) *yaml.Node {
	if root == nil || len(root.Content) == 0 {
		return nil
	}
	top := root.Content[0]
	if top == nil || top.Kind != yaml.MappingNode {
		return nil
	}
	rules := mappingGet(top, "rules")
	if rules == nil || rules.Kind != yaml.SequenceNode {
		return nil
	}
	return rules
}

// findRuleNode returns the MappingNode for a given rule id, or nil.
func findRuleNode(rulesSeq *yaml.Node, ruleID string) *yaml.Node {
	if rulesSeq == nil {
		return nil
	}
	for _, ruleNode := range rulesSeq.Content {
		if ruleNode == nil || ruleNode.Kind != yaml.MappingNode {
			continue
		}
		if mappingString(ruleNode, "id") == ruleID {
			return ruleNode
		}
	}
	return nil
}

// atomicWriteFile writes data to path via a tempfile + os.Rename for atomicity.
// Prevents concurrent hook reads from seeing a half-written YAML.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// patchRuleDisable applies (or clears) disable_until + disable_reason on one
// rule in the YAML config, preserving comments and formatting. Passing nil
// `until` clears both fields (the "enable" path).
func patchRuleDisable(yamlPath, ruleID string, until *time.Time, reason string) error {
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	rulesSeq := findRulesSeq(&root)
	if rulesSeq == nil {
		return fmt.Errorf("config has no `rules:` sequence")
	}
	ruleNode := findRuleNode(rulesSeq, ruleID)
	if ruleNode == nil {
		return fmt.Errorf("rule %q not found", ruleID)
	}
	if until != nil {
		mappingSet(ruleNode, "disable_until", until.Format(time.RFC3339))
		if reason == "" {
			mappingDelete(ruleNode, "disable_reason")
		} else {
			mappingSet(ruleNode, "disable_reason", reason)
		}
	} else {
		mappingDelete(ruleNode, "disable_until")
		mappingDelete(ruleNode, "disable_reason")
	}

	out, err := yaml.Marshal(&root)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	info, err := os.Stat(yamlPath)
	mode := os.FileMode(0o644)
	if err == nil {
		mode = info.Mode().Perm()
	}
	return atomicWriteFile(yamlPath, out, mode)
}

// ── CLI: `weiran tool-hook disable / enable / disables` ──

// handleToolHookDisable implements `disable <rule-id> --until <spec> [--reason …]`.
// `--until` is mandatory; omission is rejected so the bound is always explicit.
func handleToolHookDisable(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: tool-hook disable <rule-id> --until <+30m|RFC3339> [--reason \"…\"]")
		os.Exit(1)
	}
	ruleID := args[0]
	var untilSpec, reason string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--until":
			if i+1 < len(args) {
				untilSpec = args[i+1]
				i++
			}
		case "--reason":
			if i+1 < len(args) {
				reason = args[i+1]
				i++
			}
		case "--help", "-h":
			fmt.Print(`weiran tool-hook disable — temporarily suppress one rule.

  --until <spec>   REQUIRED. +30m | +1h30m | +2h | 2026-04-22T23:45:00-07:00
                   Hard cap: 2h per disable. Re-apply to extend.
  --reason "…"    Recommended. Shown in 'disables' listing + audit log.

The rule is skipped until the deadline; expired disables self-heal on the
next hook invocation (cleanup is automatic).
`)
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			os.Exit(1)
		}
	}

	until, err := parseUntilSpec(untilSpec, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		writeDisableOpAudit(ruleID, fmt.Sprintf("reject: %v", err))
		os.Exit(1)
	}

	cfgPath := defaultToolHookConfigPath()
	if err := patchRuleDisable(cfgPath, ruleID, &until, reason); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		writeDisableOpAudit(ruleID, fmt.Sprintf("reject: %v", err))
		os.Exit(1)
	}
	delta := time.Until(until).Round(time.Second)
	fmt.Printf("disabled %s until %s (%s)\n", ruleID, until.Format(time.RFC3339), humanizeDelta(delta))
	writeDisableOpAudit(ruleID, fmt.Sprintf("disable until=%s reason=%q", until.Format(time.RFC3339), reason))
}

// handleToolHookEnable implements `enable <rule-id>` — clears any temp disable
// early. Idempotent: succeeds even if the rule had no active disable.
func handleToolHookEnable(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: tool-hook enable <rule-id>")
		os.Exit(1)
	}
	ruleID := args[0]
	cfgPath := defaultToolHookConfigPath()
	if err := patchRuleDisable(cfgPath, ruleID, nil, ""); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		writeDisableOpAudit(ruleID, fmt.Sprintf("reject: %v", err))
		os.Exit(1)
	}
	fmt.Printf("cleared any temp disable on %s\n", ruleID)
	writeDisableOpAudit(ruleID, "enable early")
}

// handleToolHookDisables implements `disables` — list rules with a
// disable_until set. Includes expired entries so the user can clean them up.
func handleToolHookDisables(args []string) {
	cfg, err := loadToolHookConfig(defaultToolHookConfigPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	type entry struct {
		rule    *ToolHookRule
		expires time.Time
	}
	var active, expired []entry
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		if r.DisableUntil == nil {
			continue
		}
		e := entry{rule: r, expires: *r.DisableUntil}
		if time.Now().Before(e.expires) {
			active = append(active, e)
		} else {
			expired = append(expired, e)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].expires.Before(active[j].expires) })
	sort.Slice(expired, func(i, j int) bool { return expired[i].expires.After(expired[j].expires) })

	if len(active) == 0 && len(expired) == 0 {
		fmt.Println("no temp disables active")
		return
	}

	print := func(e entry) {
		delta := time.Until(e.expires).Round(time.Second)
		reason := e.rule.DisableReason
		if reason == "" {
			reason = "(no reason given)"
		}
		fmt.Printf("  %-40s  %-22s  %s\n", e.rule.ID, humanizeDelta(delta), reason)
	}
	if len(active) > 0 {
		fmt.Printf("active (%d):\n", len(active))
		for _, e := range active {
			print(e)
		}
	}
	if len(expired) > 0 {
		if len(active) > 0 {
			fmt.Println()
		}
		fmt.Printf("expired (%d) — run `tool-hook enable <id>` to tidy:\n", len(expired))
		for _, e := range expired {
			print(e)
		}
	}
}

// cleanupExpiredDisables scans cfg.Rules for rules whose DisableUntil has
// already passed and clears them from disk (yaml) + memory. This keeps the
// YAML tidy so stale "disable_until: 2020-…" entries don't pile up across
// git diffs.
//
// Called once per hook invocation, immediately after config load. Fast path
// (nothing expired) performs zero disk IO — we scan in-memory first and only
// touch the YAML when there's at least one expired entry. Each cleared rule
// gets its own audit row with event=ToolHookOp, skip_reason="auto_cleanup
// expired until=…", so the operation is observable via `tool-hook log --rule
// <id> --grep auto_cleanup`.
//
// After the cleanup, the cfg struct is mutated in place so the downstream
// eval loops see the rules as "not disabled" (the yaml disk write reaches
// future hook invocations, the memory mutation reaches the current one).
//
// Concurrency: multiple hook processes may cleanup the same rule; since we
// clear a field that's already expired, last-writer-wins is safe. The atomic
// rename in patchRuleDisable ensures no reader sees a half-written file.
func cleanupExpiredDisables(cfgPath string, cfg *ToolHookConfig, db *sql.DB) {
	if cfg == nil {
		return
	}
	now := time.Now()
	var expired []string
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		if r.DisableUntil == nil {
			continue
		}
		if r.DisableUntil.After(now) {
			continue
		}
		expired = append(expired, r.ID)
	}
	if len(expired) == 0 {
		return
	}
	for _, ruleID := range expired {
		// Per-rule patch so one malformed rule can't prevent cleaning the rest.
		// Errors on disk are logged but not fatal — hooks must not block on IO.
		var auditReason string
		if err := patchRuleDisable(cfgPath, ruleID, nil, ""); err != nil {
			auditReason = fmt.Sprintf("auto_cleanup_failed: %v", err)
			fmt.Fprintf(os.Stderr, "[tool-hook] auto-cleanup %s: %v\n", ruleID, err)
		} else {
			auditReason = "auto_cleanup expired"
		}
		// Mirror the disk change in memory so the current hook invocation's
		// eval loop treats this rule as re-enabled.
		for i := range cfg.Rules {
			if cfg.Rules[i].ID == ruleID {
				cfg.Rules[i].DisableUntil = nil
				cfg.Rules[i].DisableReason = ""
				break
			}
		}
		if db != nil {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				EventName:  "ToolHookOp",
				RuleID:     ruleID,
				Injected:   false,
				SkipReason: auditReason,
			})
		}
	}
}

// writeDisableOpAudit records a disable/enable/reject action to the audit
// table. Reuses the existing tool_hook_audit schema (no migration) by using
// a synthetic EventName so `tool-hook stats` / `log` can filter these rows.
func writeDisableOpAudit(ruleID, skipReason string) {
	db, err := openDB()
	if err != nil {
		// Don't fail the CLI command on audit write failure — the YAML is the
		// source of truth, audit is observability.
		fmt.Fprintf(os.Stderr, "[tool-hook] audit write skipped: %v\n", err)
		return
	}
	defer db.Close()
	cwd, _ := os.Getwd()
	writeToolHookAudit(db, toolHookAuditRow{
		Timestamp:  time.Now().Format(time.RFC3339),
		SessionID:  os.Getenv(strings.ToUpper(appName) + "_SESSION_ID"),
		CWD:        cwd,
		EventName:  "ToolHookOp",
		RuleID:     ruleID,
		Injected:   false,
		SkipReason: skipReason,
	})
}
