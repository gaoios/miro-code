package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractProjectDesc_Normal(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "CLAUDE.md")
	os.WriteFile(f, []byte("# CLAUDE.md\n\nThis is a test project for things.\n\n## More\n"), 0644)

	desc := extractProjectDesc(f)
	if desc != "This is a test project for things." {
		t.Errorf("desc = %q", desc)
	}
}

func TestExtractProjectDesc_SkipsBoilerplate(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "CLAUDE.md")
	content := `# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

Real description here.
`
	os.WriteFile(f, []byte(content), 0644)

	desc := extractProjectDesc(f)
	// "What This Is" is a generic heading that gets skipped; first substantive line is returned
	if desc != "Real description here." {
		t.Errorf("desc = %q, want 'Real description here.'", desc)
	}
}

func TestExtractProjectDesc_NoContent(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "CLAUDE.md")
	os.WriteFile(f, []byte("# CLAUDE.md\n\n\n"), 0644)

	desc := extractProjectDesc(f)
	if desc != "(no description)" {
		t.Errorf("desc = %q", desc)
	}
}

func TestExtractProjectDesc_NonExistent(t *testing.T) {
	desc := extractProjectDesc("/nonexistent/CLAUDE.md")
	if desc != "" {
		t.Errorf("desc = %q, want empty", desc)
	}
}

func TestExtractProjectDesc_LongLine(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "CLAUDE.md")
	longLine := strings.Repeat("x", 200)
	os.WriteFile(f, []byte(longLine+"\n"), 0644)

	desc := extractProjectDesc(f)
	if len(desc) > 125 { // 120 + "…"
		t.Errorf("desc should be truncated, got %d chars", len(desc))
	}
}

func TestFindCLAUDEMDs_Depth(t *testing.T) {
	dir := t.TempDir()

	// root level
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("root"), 0644)

	// depth 1
	sub1 := filepath.Join(dir, "sub1")
	os.MkdirAll(sub1, 0755)
	os.WriteFile(filepath.Join(sub1, "CLAUDE.md"), []byte("sub1"), 0644)

	// depth 2
	sub2 := filepath.Join(sub1, "sub2")
	os.MkdirAll(sub2, 0755)
	os.WriteFile(filepath.Join(sub2, "CLAUDE.md"), []byte("sub2"), 0644)

	// depth 3 — should NOT be found with maxDepth=2
	sub3 := filepath.Join(sub2, "sub3")
	os.MkdirAll(sub3, 0755)
	os.WriteFile(filepath.Join(sub3, "CLAUDE.md"), []byte("sub3"), 0644)

	results := findCLAUDEMDs(dir, 2)
	if len(results) != 3 { // root, sub1, sub2
		t.Errorf("expected 3 CLAUDE.md files, got %d", len(results))
		for _, r := range results {
			t.Logf("  found: %s", r)
		}
	}
}

func TestFindCLAUDEMDs_SkipsNodeModules(t *testing.T) {
	dir := t.TempDir()

	nm := filepath.Join(dir, "node_modules", "pkg")
	os.MkdirAll(nm, 0755)
	os.WriteFile(filepath.Join(nm, "CLAUDE.md"), []byte("should skip"), 0644)

	git := filepath.Join(dir, ".git")
	os.MkdirAll(git, 0755)
	os.WriteFile(filepath.Join(git, "CLAUDE.md"), []byte("should skip"), 0644)

	results := findCLAUDEMDs(dir, 2)
	if len(results) != 0 {
		t.Errorf("expected 0 results (all in excluded dirs), got %d", len(results))
	}
}

func TestFindCLAUDEMDs_NegativeDepth(t *testing.T) {
	dir := t.TempDir()
	results := findCLAUDEMDs(dir, -1)
	if len(results) != 0 {
		t.Errorf("negative depth should return empty, got %d", len(results))
	}
}

func TestFindCLAUDEMDs_PrefersLocal(t *testing.T) {
	dir := t.TempDir()

	// Create both CLAUDE.md and CLAUDE.local.md
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("shared desc"), 0644)
	os.WriteFile(filepath.Join(dir, "CLAUDE.local.md"), []byte("local override"), 0644)

	results := findCLAUDEMDs(dir, 0)
	if len(results) != 1 {
		t.Fatalf("expected 1 result (local preferred), got %d", len(results))
	}
	if !strings.HasSuffix(results[0], "CLAUDE.local.md") {
		t.Errorf("expected CLAUDE.local.md, got %s", results[0])
	}

	// Verify the description comes from local
	desc := extractProjectDesc(results[0])
	if desc != "local override" {
		t.Errorf("expected 'local override', got '%s'", desc)
	}
}

func TestFindCLAUDEMDs_FallbackToRegular(t *testing.T) {
	dir := t.TempDir()

	// Only CLAUDE.md, no local
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("shared desc"), 0644)

	results := findCLAUDEMDs(dir, 0)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !strings.HasSuffix(results[0], "CLAUDE.md") {
		t.Errorf("expected CLAUDE.md, got %s", results[0])
	}
}

func TestBuildProjectIndex_NotEmpty(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "CLAUDE.md"), []byte("Test project"), 0644); err != nil {
		t.Fatal(err)
	}
	origHome := home
	origProjectRoots := projectRoots
	home = dir
	projectRoots = []string{projectDir}
	t.Cleanup(func() {
		home = origHome
		projectRoots = origProjectRoots
	})

	idx := buildProjectIndex()
	if idx == "" {
		t.Fatal("project index is empty")
	}
	if !strings.Contains(idx, "| Path |") {
		t.Error("missing table header")
	}
}

func TestBuildProjectIndex_NoDuplicates(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "CLAUDE.md"), []byte("Test project"), 0644); err != nil {
		t.Fatal(err)
	}
	origHome := home
	origProjectRoots := projectRoots
	home = dir
	projectRoots = []string{projectDir}
	t.Cleanup(func() {
		home = origHome
		projectRoots = origProjectRoots
	})

	idx := buildProjectIndex()
	lines := strings.Split(idx, "\n")
	seen := make(map[string]bool)
	for _, l := range lines {
		if !strings.HasPrefix(l, "| `") {
			continue
		}
		parts := strings.SplitN(l, "|", 4)
		if len(parts) < 3 {
			continue
		}
		path := strings.TrimSpace(parts[1])
		if seen[path] {
			t.Errorf("duplicate project: %s", path)
		}
		seen[path] = true
	}
}
