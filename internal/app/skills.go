package app

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// skippedDirs are directories excluded from recursive CLAUDE.md/skill scanning
var skippedDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	"vendor":       true,
	"dist":         true,
	"__pycache__":  true,
	".venv":        true,
	"build":        true,
}

func isSkippedDir(name string) bool {
	return skippedDirs[name]
}

// ── Skill Index ──

type skillEntry struct {
	Name        string
	Description string
	Dir         string // source directory
	Modes       string // "all", "interactive", "interactive,evolve", "" (= all)
}

// parseSkillFrontmatter extracts name, description, and modes from SKILL.md
// supports YAML frontmatter and fallback (extract from # heading + first paragraph)
// modes: "all", "interactive", "interactive,evolve", "" (= all)
func parseSkillFrontmatter(path string) (name, desc, modes string) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inFrontmatter := false
	hasFrontmatter := false
	inDesc := false
	var descLines []string
	var fallbackTitle, fallbackDesc string

	for scanner.Scan() {
		line := scanner.Text()

		// frontmatter handling
		if line == "---" {
			if !hasFrontmatter && !inFrontmatter {
				inFrontmatter = true
				hasFrontmatter = true
				continue
			}
			if inFrontmatter {
				inFrontmatter = false
				continue
			}
		}
		if inFrontmatter {
			if strings.HasPrefix(line, "name:") {
				name = strings.TrimSpace(strings.TrimPrefix(line, "name:"))
				inDesc = false
				continue
			}
			if strings.HasPrefix(line, "modes:") {
				modes = strings.TrimSpace(strings.TrimPrefix(line, "modes:"))
				modes = strings.Trim(modes, "\"'")
				inDesc = false
				continue
			}
			if strings.HasPrefix(line, "description:") {
				rest := strings.TrimSpace(strings.TrimPrefix(line, "description:"))
				if rest != "" && rest != "|" {
					desc = strings.Trim(rest, "\"'")
					continue
				}
				inDesc = true
				continue
			}
			if inDesc {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" || (!strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t")) {
					inDesc = false
					continue
				}
				descLines = append(descLines, trimmed)
				continue
			}
			if strings.Contains(line, ":") && !strings.HasPrefix(line, " ") {
				inDesc = false
			}
			continue
		}

		// non-frontmatter area: extract fallback title + first paragraph
		if fallbackTitle == "" && strings.HasPrefix(line, "# ") {
			fallbackTitle = strings.TrimSpace(strings.TrimPrefix(line, "# "))
			// strip possible "Skill" / " — " suffix
			if idx := strings.Index(fallbackTitle, " — "); idx > 0 {
				fallbackDesc = strings.TrimSpace(fallbackTitle[idx+len(" — "):])
				fallbackTitle = fallbackTitle[:idx]
			}
			continue
		}
		// use first non-empty paragraph after title as fallback desc
		if fallbackTitle != "" && fallbackDesc == "" {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "|") && !strings.HasPrefix(trimmed, "```") {
				fallbackDesc = trimmed
			}
		}
	}

	if len(descLines) > 0 {
		desc = strings.Join(descLines, " ")
	}
	if name == "" {
		name = fallbackTitle
	}
	if desc == "" {
		desc = fallbackDesc
	}
	return
}

// buildSkillIndex scans all skill directories and builds a deduplicated index
func buildSkillIndex() string {
	seen := make(map[string]bool)
	var skills []skillEntry

	for _, dir := range skillDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			skillName := e.Name()
			if seen[skillName] {
				continue
			}

			skillMd := filepath.Join(dir, skillName, "SKILL.md")
			if _, err := os.Stat(skillMd); err != nil {
				continue
			}

			name, desc, modes := parseSkillFrontmatter(skillMd)
			if name == "" {
				name = skillName
			}

			// Mode filtering: skip skills not intended for current mode
			if modes != "" && modes != "all" && currentMode != "" {
				allowed := false
				for _, m := range strings.Split(modes, ",") {
					if strings.TrimSpace(m) == currentMode {
						allowed = true
						break
					}
				}
				if !allowed {
					continue
				}
			}

			// truncate description: strip trigger conditions, keep only feature description
			// "触发" is the Chinese equivalent of "Trigger" — supports bilingual SKILL.md files
			for _, cut := range []string{"Trigger", "trigger", "触发"} {
				if idx := strings.Index(desc, cut); idx > 0 {
					desc = strings.TrimSpace(desc[:idx])
					desc = strings.TrimRight(desc, "。.\n :：,，")
					break
				}
			}
			if runes := []rune(desc); len(runes) > 100 {
				desc = string(runes[:100]) + "…"
			}

			seen[skillName] = true
			skills = append(skills, skillEntry{Name: name, Description: desc, Dir: dir, Modes: modes})
		}
	}

	if len(skills) == 0 {
		return ""
	}

	// Group skills by directory for compact output
	dirSkills := make(map[string][]string)
	var dirOrder []string
	for _, s := range skills {
		if _, exists := dirSkills[s.Dir]; !exists {
			dirOrder = append(dirOrder, s.Dir)
		}
		dirSkills[s.Dir] = append(dirSkills[s.Dir], s.Name)
	}

	var b strings.Builder
	b.WriteString("## Available Skills\n\n")
	for _, dir := range dirOrder {
		b.WriteString(fmt.Sprintf("- %s\n  %s\n", dir, strings.Join(dirSkills[dir], ", ")))
	}
	return b.String()
}

// ── Project Index ──

// projectRoots is the list of root directories to scan for CLAUDE.md, populated after initWorkspace
var projectRoots []string

type projectEntry struct {
	Path string
	Desc string
}

// extractProjectDesc extracts a one-line description from CLAUDE.md or CLAUDE.local.md
// uses safeReadFile to reject symlinked files
func extractProjectDesc(path string) string {
	data, err := safeReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	// find the first line with substantive content (skip blanks, "# CLAUDE.md" heading, boilerplate)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "# CLAUDE.md" || strings.Contains(line, "provides guidance to Claude Code") {
			continue
		}
		// strip markdown heading prefix
		line = strings.TrimLeft(line, "# ")
		// skip generic headings that don't describe the project
		lower := strings.ToLower(line)
		if lower == "what this is" || lower == "what is this" || lower == "overview" || lower == "project overview" {
			continue
		}
		if len(line) > 120 {
			line = line[:120] + "…"
		}
		return line
	}
	return "(no description)"
}

// findCLAUDEMDs recursively scans a directory (up to maxDepth levels) for CLAUDE.md
// uses a visited set to prevent infinite recursion from symlink loops
func findCLAUDEMDs(root string, maxDepth int) []string {
	visited := make(map[deviceInode]bool)
	return findCLAUDEMDsInner(root, maxDepth, visited)
}

func findCLAUDEMDsInner(root string, maxDepth int, visited map[deviceInode]bool) []string {
	var results []string
	if maxDepth < 0 {
		return results
	}

	// loop prevention: check if inode has been visited
	if di, ok := getDeviceInode(root); ok {
		if visited[di] {
			return results // loop detected, skip
		}
		visited[di] = true
	}

	// Prefer CLAUDE.local.md over CLAUDE.md (local overrides shared, like Claude Code itself)
	localCandidate := filepath.Join(root, "CLAUDE.local.md")
	candidate := filepath.Join(root, "CLAUDE.md")
	if info, err := lstatSafe(localCandidate); err == nil && !info.IsDir() {
		results = append(results, localCandidate)
	} else if info, err := lstatSafe(candidate); err == nil && !info.IsDir() {
		results = append(results, candidate)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return results
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if isSkippedDir(name) {
			continue
		}
		subDir := filepath.Join(root, name)
		// skip symlink directories
		if isSymlink(subDir) {
			continue
		}
		results = append(results, findCLAUDEMDsInner(subDir, maxDepth-1, visited)...)
	}
	return results
}

func buildProjectIndex() string {
	seen := make(map[string]bool)
	var projects []projectEntry

	// special case: ~/CLAUDE.md (project routing table)
	homeClaude := filepath.Join(home, "CLAUDE.md")
	if _, err := os.Stat(homeClaude); err == nil {
		projects = append(projects, projectEntry{Path: "~/CLAUDE.md", Desc: "Project routing table (cd quick reference)"})
		seen[homeClaude] = true
	}

	for _, root := range projectRoots {
		if _, err := os.Stat(root); err != nil {
			continue
		}
		// for directories that are projects themselves (e.g. familydash), only scan self
		// Prefer CLAUDE.local.md over CLAUDE.md
		candidate := filepath.Join(root, "CLAUDE.local.md")
		if _, err := os.Stat(candidate); err != nil {
			candidate = filepath.Join(root, "CLAUDE.md")
		}
		if _, err := os.Stat(candidate); err == nil && !seen[candidate] {
			seen[candidate] = true
			shortPath := strings.Replace(candidate, home, "~", 1)
			projects = append(projects, projectEntry{Path: shortPath, Desc: extractProjectDesc(candidate)})
		}

		// scan subdirectories (up to 2 levels deep)
		for _, f := range findCLAUDEMDs(root, 2) {
			if seen[f] {
				continue
			}
			seen[f] = true
			shortPath := strings.Replace(f, home, "~", 1)
			projects = append(projects, projectEntry{Path: shortPath, Desc: extractProjectDesc(f)})
		}
	}

	if len(projects) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Project Context Index\n\n")
	b.WriteString("The following projects have CLAUDE.md. When working on a project, `Read` its CLAUDE.md first for full context.\n\n")
	b.WriteString("| Path | Description |\n|------|-------------|\n")
	for _, p := range projects {
		fmt.Fprintf(&b, "| `%s` | %s |\n", p.Path, p.Desc)
	}
	return b.String()
}
