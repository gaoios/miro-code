package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// fixtureYAML is a minimal tool-hooks.yaml with comments. The tests assert
// that patching one rule's disable fields preserves every comment line — that's
// the whole point of using yaml.v3 Node API instead of Marshal-the-struct.
const fixtureYAML = `# Header comment — must survive patching.
# Second header line.

budget: 1500

rules:
  # Rule ONE — important reflex.
  - id: rule_one
    tools: [Edit, Write]
    match:
      - "**/*.md"
    priority: 80
    inject: |
      First rule injection.

  # Rule TWO — another one.
  - id: rule_two
    tools: [Bash]
    match_input:
      - "\\bkubectl\\b"
    priority: 50
    inject: |
      Second rule injection.
`

func writeFixture(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "tool-hooks.yaml")
	if err := os.WriteFile(path, []byte(fixtureYAML), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir, path
}

// ── parseUntilSpec ──

func TestParseUntilSpec_Relative(t *testing.T) {
	now := time.Date(2026, 4, 22, 23, 0, 0, 0, time.UTC)
	cases := []struct {
		spec  string
		want  time.Duration
		valid bool
	}{
		{"+30m", 30 * time.Minute, true},
		{"+1h", 1 * time.Hour, true},
		{"+1h30m", 90 * time.Minute, true},
		{"+2h", 2 * time.Hour, true},
		{"+120m", 120 * time.Minute, true},
	}
	for _, c := range cases {
		got, err := parseUntilSpec(c.spec, now)
		if err != nil {
			t.Errorf("parseUntilSpec(%q): unexpected error: %v", c.spec, err)
			continue
		}
		if d := got.Sub(now); d != c.want {
			t.Errorf("parseUntilSpec(%q) = +%s, want +%s", c.spec, d, c.want)
		}
	}
}

func TestParseUntilSpec_AbsoluteRFC3339(t *testing.T) {
	now := time.Date(2026, 4, 22, 23, 0, 0, 0, time.UTC)
	spec := now.Add(45 * time.Minute).Format(time.RFC3339)
	got, err := parseUntilSpec(spec, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Sub(now) != 45*time.Minute {
		t.Errorf("got %s, want 45m from now", got.Sub(now))
	}
}

func TestParseUntilSpec_Rejects(t *testing.T) {
	now := time.Date(2026, 4, 22, 23, 0, 0, 0, time.UTC)
	cases := []struct {
		spec   string
		errSub string
	}{
		{"", "required"},
		{"30m", "prefix with +"},        // common mistake: no +
		{"+3h", "exceeds"},              // over 2h cap
		{"+2h1m", "exceeds"},            // just over
		{"-5m", "negative"},             // negative
		{"+0s", "future"},               // zero duration
		{"+-1m", "future"},              // +(-1m) parses as a negative duration
		{"+garbage", "invalid duration"},
		{"not-a-time", "invalid"},       // neither duration nor RFC3339
	}
	for _, c := range cases {
		_, err := parseUntilSpec(c.spec, now)
		if err == nil {
			t.Errorf("parseUntilSpec(%q): expected error, got nil", c.spec)
			continue
		}
		if !strings.Contains(err.Error(), c.errSub) {
			t.Errorf("parseUntilSpec(%q): error %q does not contain %q", c.spec, err, c.errSub)
		}
	}
}

// ── patchRuleDisable ──

func TestPatchRuleDisable_AddsFieldsAndPreservesComments(t *testing.T) {
	_, path := writeFixture(t)
	until := time.Date(2026, 4, 22, 23, 30, 0, 0, time.UTC)
	if err := patchRuleDisable(path, "rule_one", &until, "testing"); err != nil {
		t.Fatalf("patch: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)

	// Every comment from the fixture must still be present.
	for _, c := range []string{
		"# Header comment — must survive patching.",
		"# Second header line.",
		"# Rule ONE — important reflex.",
		"# Rule TWO — another one.",
	} {
		if !strings.Contains(s, c) {
			t.Errorf("comment %q was lost after patch", c)
		}
	}
	// The new fields must be present on rule_one.
	if !strings.Contains(s, "disable_until: \"2026-04-22T23:30:00Z\"") &&
		!strings.Contains(s, "disable_until: 2026-04-22T23:30:00Z") {
		t.Errorf("disable_until not written. File:\n%s", s)
	}
	if !strings.Contains(s, "disable_reason: testing") &&
		!strings.Contains(s, `disable_reason: "testing"`) {
		t.Errorf("disable_reason not written. File:\n%s", s)
	}

	// Reload via the normal config loader and verify the struct round-trips.
	cfg, err := loadToolHookConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	var found *ToolHookRule
	for i := range cfg.Rules {
		if cfg.Rules[i].ID == "rule_one" {
			found = &cfg.Rules[i]
			break
		}
	}
	if found == nil {
		t.Fatal("rule_one missing after reload")
	}
	if found.DisableUntil == nil {
		t.Fatal("DisableUntil nil after reload")
	}
	if !found.DisableUntil.Equal(until) {
		t.Errorf("DisableUntil = %s, want %s", found.DisableUntil, until)
	}
	if found.DisableReason != "testing" {
		t.Errorf("DisableReason = %q, want %q", found.DisableReason, "testing")
	}
	// Unaffected rule stays untouched.
	for i := range cfg.Rules {
		if cfg.Rules[i].ID == "rule_two" {
			if cfg.Rules[i].DisableUntil != nil {
				t.Errorf("rule_two was contaminated")
			}
			break
		}
	}
}

func TestPatchRuleDisable_ClearsFields(t *testing.T) {
	_, path := writeFixture(t)
	until := time.Date(2026, 4, 22, 23, 30, 0, 0, time.UTC)
	if err := patchRuleDisable(path, "rule_one", &until, "testing"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := patchRuleDisable(path, "rule_one", nil, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if strings.Contains(s, "disable_until") {
		t.Errorf("disable_until should be gone. File:\n%s", s)
	}
	if strings.Contains(s, "disable_reason") {
		t.Errorf("disable_reason should be gone. File:\n%s", s)
	}
	// Header comments still intact.
	if !strings.Contains(s, "# Header comment — must survive patching.") {
		t.Error("header comment lost")
	}
}

func TestPatchRuleDisable_UnknownRule(t *testing.T) {
	_, path := writeFixture(t)
	until := time.Date(2026, 4, 22, 23, 30, 0, 0, time.UTC)
	err := patchRuleDisable(path, "does_not_exist", &until, "x")
	if err == nil {
		t.Fatal("expected error for unknown rule")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error does not mention 'not found': %v", err)
	}
}

// ── isTempDisabled ──

func TestIsTempDisabled(t *testing.T) {
	future := time.Now().Add(10 * time.Minute)
	past := time.Now().Add(-10 * time.Minute)

	cases := []struct {
		name string
		r    ToolHookRule
		want bool
	}{
		{"nil DisableUntil", ToolHookRule{}, false},
		{"future", ToolHookRule{DisableUntil: &future}, true},
		{"past (expired, self-heals)", ToolHookRule{DisableUntil: &past}, false},
	}
	for _, c := range cases {
		if got := isTempDisabled(&c.r); got != c.want {
			t.Errorf("%s: isTempDisabled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTempDisableReasonFormat(t *testing.T) {
	at := time.Date(2026, 4, 22, 23, 30, 0, 0, time.UTC)
	r := ToolHookRule{DisableUntil: &at, DisableReason: "testing"}
	got := tempDisableReason(&r)
	if !strings.HasPrefix(got, "temp_disabled until=") {
		t.Errorf("bad prefix: %q", got)
	}
	if !strings.Contains(got, `reason="testing"`) {
		t.Errorf("reason not quoted: %q", got)
	}
	if !strings.Contains(got, "2026-04-22T23:30:00Z") {
		t.Errorf("timestamp missing: %q", got)
	}
}

// ── mapping helpers ──

func TestMappingHelpers(t *testing.T) {
	src := `
id: foo
priority: 50
disabled: false
`
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(src), &root); err != nil {
		t.Fatal(err)
	}
	m := root.Content[0]
	if mappingString(m, "id") != "foo" {
		t.Error("get id")
	}
	if mappingString(m, "missing") != "" {
		t.Error("get missing")
	}
	mappingSet(m, "disable_until", "2026-04-22T23:30:00Z")
	if mappingString(m, "disable_until") != "2026-04-22T23:30:00Z" {
		t.Error("set new key")
	}
	mappingSet(m, "priority", "99") // update existing
	if mappingString(m, "priority") != "99" {
		t.Error("update existing key")
	}
	if !mappingDelete(m, "disable_until") {
		t.Error("delete existing")
	}
	if mappingString(m, "disable_until") != "" {
		t.Error("still present after delete")
	}
	if mappingDelete(m, "never_was_here") {
		t.Error("delete missing should return false")
	}
}

// ── cleanupExpiredDisables ──

func TestCleanupExpiredDisables_ClearsOldEntries(t *testing.T) {
	// Fixture with one already-expired disable on rule_one.
	_, path := writeFixture(t)
	past := time.Now().Add(-5 * time.Minute)
	if err := patchRuleDisable(path, "rule_one", &past, "stale"); err != nil {
		t.Fatalf("seed expired disable: %v", err)
	}

	cfg, err := loadToolHookConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: rule_one has the expired disable set.
	var r1 *ToolHookRule
	for i := range cfg.Rules {
		if cfg.Rules[i].ID == "rule_one" {
			r1 = &cfg.Rules[i]
		}
	}
	if r1 == nil || r1.DisableUntil == nil {
		t.Fatal("setup: rule_one should have DisableUntil")
	}

	cleanupExpiredDisables(path, cfg, nil) // db=nil is fine

	// In-memory: DisableUntil cleared.
	for i := range cfg.Rules {
		if cfg.Rules[i].ID == "rule_one" && cfg.Rules[i].DisableUntil != nil {
			t.Error("in-memory DisableUntil not cleared")
		}
	}
	// On-disk: fields removed.
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "disable_until") {
		t.Errorf("yaml still contains disable_until:\n%s", raw)
	}
}

func TestCleanupExpiredDisables_LeavesFutureAlone(t *testing.T) {
	_, path := writeFixture(t)
	future := time.Now().Add(30 * time.Minute)
	if err := patchRuleDisable(path, "rule_one", &future, "active"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := loadToolHookConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupExpiredDisables(path, cfg, nil)

	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "disable_until") {
		t.Errorf("future disable was incorrectly cleared:\n%s", raw)
	}
}

func TestCleanupExpiredDisables_NoOpWhenNothing(t *testing.T) {
	_, path := writeFixture(t)
	before, _ := os.ReadFile(path)
	cfg, err := loadToolHookConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupExpiredDisables(path, cfg, nil)
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("yaml was modified on no-op cleanup:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
