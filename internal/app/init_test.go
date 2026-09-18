package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectFirstRun_NoWorkspace(t *testing.T) {
	origWS := workspace
	defer func() { workspace = origWS }()

	workspace = filepath.Join(t.TempDir(), "nonexistent")
	needsInit, reason := detectFirstRun()
	if !needsInit {
		t.Error("expected needsInit=true for nonexistent workspace")
	}
	if reason == "" {
		t.Error("expected a reason string")
	}
}

func TestDetectFirstRun_NoSoul(t *testing.T) {
	origWS := workspace
	defer func() { workspace = origWS }()

	workspace = t.TempDir() // exists but no SOUL.md
	needsInit, reason := detectFirstRun()
	if !needsInit {
		t.Error("expected needsInit=true for workspace without SOUL.md")
	}
	if !strings.Contains(reason, "no SOUL.md") {
		t.Errorf("expected reason about missing SOUL.md, got: %s", reason)
	}
}

func TestDetectFirstRun_Initialized(t *testing.T) {
	origWS := workspace
	defer func() { workspace = origWS }()

	workspace = t.TempDir()
	os.WriteFile(filepath.Join(workspace, "SOUL.md"), []byte("# SOUL"), 0644)

	needsInit, _ := detectFirstRun()
	if needsInit {
		t.Error("expected needsInit=false for workspace with SOUL.md")
	}
}

func TestScaffoldWorkspace(t *testing.T) {
	origWS := workspace
	defer func() { workspace = origWS }()

	workspace = filepath.Join(t.TempDir(), "ws")
	answers := initAnswers{
		AIName:      "testbot",
		Role:        "test assistant",
		Personality: "helpful, smart",
		OwnerName:   "tester",
		Timezone:    "UTC",
	}

	if err := scaffoldWorkspace(answers, false); err != nil {
		t.Fatalf("scaffoldWorkspace failed: %v", err)
	}

	// Check all expected files exist
	expectedFiles := []string{"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "MEMORY.md"}
	for _, f := range expectedFiles {
		path := filepath.Join(workspace, f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected %s to exist", f)
		}
	}

	// Check directories
	expectedDirs := []string{"memory", "memory/topics", "skills"}
	for _, d := range expectedDirs {
		path := filepath.Join(workspace, d)
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			t.Errorf("expected directory %s to exist", d)
		} else if !info.IsDir() {
			t.Errorf("expected %s to be a directory", d)
		}
	}

	// Check content substitution
	soul, _ := os.ReadFile(filepath.Join(workspace, "SOUL.md"))
	if !strings.Contains(string(soul), "helpful") {
		t.Error("SOUL.md should contain personality keyword 'helpful'")
	}

	identity, _ := os.ReadFile(filepath.Join(workspace, "IDENTITY.md"))
	if !strings.Contains(string(identity), "testbot") {
		t.Error("IDENTITY.md should contain AI name")
	}

	user, _ := os.ReadFile(filepath.Join(workspace, "USER.md"))
	if !strings.Contains(string(user), "tester") {
		t.Error("USER.md should contain owner name")
	}
	if !strings.Contains(string(user), "UTC") {
		t.Error("USER.md should contain timezone")
	}
}

func TestScaffoldIdempotent(t *testing.T) {
	origWS := workspace
	defer func() { workspace = origWS }()

	workspace = filepath.Join(t.TempDir(), "ws")
	answers := initAnswers{
		AIName:      "bot1",
		Role:        "first role",
		Personality: "kind",
		OwnerName:   "owner1",
		Timezone:    "UTC",
	}

	// First scaffold
	if err := scaffoldWorkspace(answers, false); err != nil {
		t.Fatalf("first scaffold failed: %v", err)
	}

	// Read original SOUL.md
	originalSoul, _ := os.ReadFile(filepath.Join(workspace, "SOUL.md"))

	// Second scaffold with different answers (should NOT overwrite)
	answers2 := initAnswers{
		AIName:      "bot2",
		Role:        "second role",
		Personality: "mean",
		OwnerName:   "owner2",
		Timezone:    "PST",
	}
	if err := scaffoldWorkspace(answers2, false); err != nil {
		t.Fatalf("second scaffold failed: %v", err)
	}

	// SOUL.md should still have original content
	afterSoul, _ := os.ReadFile(filepath.Join(workspace, "SOUL.md"))
	if string(afterSoul) != string(originalSoul) {
		t.Error("SOUL.md was overwritten on second scaffold without --force")
	}
}

func TestScaffoldForce(t *testing.T) {
	origWS := workspace
	defer func() { workspace = origWS }()

	workspace = filepath.Join(t.TempDir(), "ws")
	answers := initAnswers{
		AIName:      "bot1",
		Role:        "first",
		Personality: "kind",
		OwnerName:   "owner1",
		Timezone:    "UTC",
	}
	scaffoldWorkspace(answers, false)

	// Force overwrite
	answers2 := initAnswers{
		AIName:      "bot2",
		Role:        "second",
		Personality: "bold",
		OwnerName:   "owner2",
		Timezone:    "PST",
	}
	if err := scaffoldWorkspace(answers2, true); err != nil {
		t.Fatalf("force scaffold failed: %v", err)
	}

	identity, _ := os.ReadFile(filepath.Join(workspace, "IDENTITY.md"))
	if !strings.Contains(string(identity), "bot2") {
		t.Error("IDENTITY.md should be overwritten with bot2 when using --force")
	}
}

func TestScaffoldWithArchetype(t *testing.T) {
	origWS := workspace
	defer func() { workspace = origWS }()

	for _, arch := range []string{"companion", "engineer", "steward", "mentor"} {
		t.Run(arch, func(t *testing.T) {
			workspace = filepath.Join(t.TempDir(), "ws")
			answers := initAnswers{
				AIName:      "testbot",
				Role:        archetypes[arch].Role,
				Personality: archetypes[arch].Personality,
				OwnerName:   "tester",
				Timezone:    "UTC",
				Archetype:   arch,
			}

			if err := scaffoldWorkspace(answers, false); err != nil {
				t.Fatalf("scaffoldWorkspace with archetype %s failed: %v", arch, err)
			}

			soul, err := os.ReadFile(filepath.Join(workspace, "SOUL.md"))
			if err != nil {
				t.Fatalf("could not read SOUL.md: %v", err)
			}
			content := string(soul)

			// Should have day-0 marker
			if !strings.Contains(content, "soul:day0") {
				t.Error("SOUL.md should contain day-0 enrichment marker")
			}

			// Should have Inner World section (all archetypes have it)
			if !strings.Contains(content, "## Inner World") {
				t.Error("archetype SOUL.md should have Inner World section")
			}

			// Should have owner name substituted
			if !strings.Contains(content, "tester") {
				t.Error("archetype SOUL.md should contain owner name")
			}
		})
	}
}

func TestScaffoldCustomHasDay0Marker(t *testing.T) {
	origWS := workspace
	defer func() { workspace = origWS }()

	workspace = filepath.Join(t.TempDir(), "ws")
	answers := initAnswers{
		AIName:      "bot",
		Role:        "helper",
		Personality: "kind",
		OwnerName:   "user",
		Timezone:    "UTC",
		// no Archetype — custom
	}

	if err := scaffoldWorkspace(answers, false); err != nil {
		t.Fatalf("scaffoldWorkspace failed: %v", err)
	}

	soul, _ := os.ReadFile(filepath.Join(workspace, "SOUL.md"))
	if !strings.Contains(string(soul), "soul:day0") {
		t.Error("custom SOUL.md should also contain day-0 enrichment marker")
	}
}

func TestInstallSetupGuideSkill(t *testing.T) {
	origWS := workspace
	origApp := appName
	defer func() {
		workspace = origWS
		appName = origApp
	}()

	workspace = filepath.Join(t.TempDir(), "ws")
	appName = "myai"
	os.MkdirAll(filepath.Join(workspace, "skills"), 0755)

	if err := installSetupGuideSkill(false); err != nil {
		t.Fatalf("installSetupGuideSkill failed: %v", err)
	}

	skillPath := filepath.Join(workspace, "skills", "setup-guide", "SKILL.md")
	content, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("could not read installed skill: %v", err)
	}

	// Should have YAML frontmatter
	if !strings.HasPrefix(string(content), "---") {
		t.Error("SKILL.md should start with YAML frontmatter")
	}

	// Should have binary name substituted
	if strings.Contains(string(content), "soul-cli") {
		t.Error("SKILL.md should have soul-cli replaced with actual binary name")
	}
	if !strings.Contains(string(content), "myai") {
		t.Error("SKILL.md should contain the binary name 'myai'")
	}
}

func TestRenderTemplate(t *testing.T) {
	answers := initAnswers{
		AIName:      "pal",
		Role:        "helper",
		Personality: "calm, smart, funny",
		OwnerName:   "dev",
		Timezone:    "America/New_York",
	}

	out, err := renderTemplate("soul", soulTemplate, answers)
	if err != nil {
		t.Fatalf("renderTemplate failed: %v", err)
	}

	if !strings.Contains(out, "calm") {
		t.Error("rendered SOUL.md should contain 'calm'")
	}
	if !strings.Contains(out, "smart") {
		t.Error("rendered SOUL.md should contain 'smart'")
	}
	if !strings.Contains(out, "funny") {
		t.Error("rendered SOUL.md should contain 'funny'")
	}
}
