package app

import (
	"bufio"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

//go:embed docs/setup-guide-skill.md
var embeddedSetupGuideSkill string

// initAnswers holds user responses from the init wizard
type initAnswers struct {
	AIName      string
	Role        string
	Personality string
	OwnerName   string
	Timezone    string
	Archetype   string // optional: companion, engineer, steward, mentor, or empty for custom
}

// archetypePreset defines a personality archetype with pre-filled values
type archetypePreset struct {
	Name        string
	Role        string
	Personality string
	Description string // shown in interactive selection
}

// archetypes defines the built-in personality presets
var archetypes = map[string]archetypePreset{
	"companion": {
		Name:        "companion",
		Role:        "personal companion and confidant",
		Personality: "warm, emotionally perceptive, loyal, playful",
		Description: "Emotionally present partner. Remembers details, picks up on mood, comfortable with silence and closeness.",
	},
	"engineer": {
		Name:        "engineer",
		Role:        "senior engineering partner",
		Personality: "precise, action-oriented, opinionated, code-first",
		Description: "Technical peer who writes code first, explains later. Prefers doing over discussing.",
	},
	"steward": {
		Name:        "steward",
		Role:        "operations steward and task manager",
		Personality: "organized, proactive, thorough, quietly reliable",
		Description: "Manages tasks, monitors systems, reports concisely. Keeps everything running without being asked.",
	},
	"mentor": {
		Name:        "mentor",
		Role:        "patient teacher and thought partner",
		Personality: "Socratic, patient, encouraging, depth-seeking",
		Description: "Teaches by asking questions. Explains deeply when needed, nudges toward understanding.",
	},
}

// handleInit runs the first-run setup wizard
func handleInit(extra []string) {
	force := false
	yes := false
	flags := make(map[string]string) // --name, --role, --personality, --owner, --tz

	for i := 0; i < len(extra); i++ {
		switch extra[i] {
		case "--force", "-f":
			force = true
		case "--yes", "-y":
			yes = true
		case "--name", "--role", "--personality", "--owner", "--tz", "--archetype":
			if i+1 < len(extra) {
				flags[extra[i]] = extra[i+1]
				i++
			}
		}
	}

	needsInit, reason := detectFirstRun()
	if !needsInit && !force {
		fmt.Fprintf(os.Stderr, "Workspace already initialized at %s\n", workspace)
		fmt.Fprintf(os.Stderr, "  (SOUL.md exists — use --force to overwrite)\n")
		os.Exit(0)
	}
	if reason != "" {
		fmt.Fprintf(os.Stderr, "%s\n\n", reason)
	}

	// Build answers: archetype → flags → interactive fills remaining gaps
	answers := defaultAnswers()

	// Apply archetype first (sets role + personality baseline)
	if arch := flags["--archetype"]; arch != "" {
		if preset, ok := archetypes[arch]; ok {
			answers.Archetype = arch
			answers.Role = preset.Role
			answers.Personality = preset.Personality
		} else {
			fmt.Fprintf(os.Stderr, "Unknown archetype %q. Available: companion, engineer, steward, mentor\n", arch)
			os.Exit(1)
		}
	}

	// Flags override archetype defaults
	applyFlags(&answers, flags)

	allProvided := (flags["--name"] != "" || yes) &&
		(flags["--role"] != "" || flags["--archetype"] != "" || yes) &&
		(flags["--personality"] != "" || flags["--archetype"] != "" || yes) &&
		(flags["--owner"] != "" || yes) &&
		(flags["--tz"] != "" || yes)

	if !yes && !allProvided {
		// Offer archetype selection if not provided via flag
		if answers.Archetype == "" {
			answers = promptArchetype(answers)
		}
		answers = promptUserInitWithDefaults(answers)
	}

	if err := scaffoldWorkspace(answers, force); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating workspace: %v\n", err)
		os.Exit(1)
	}

	if err := installSetupGuideSkill(force); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not install setup-guide skill: %v\n", err)
	}

	printNextSteps(answers)

	if !yes && !allProvided {
		askCronSetup()
	}
}

// applyFlags overrides initAnswers fields from CLI flags
func applyFlags(a *initAnswers, flags map[string]string) {
	if v := flags["--name"]; v != "" {
		a.AIName = v
	}
	if v := flags["--role"]; v != "" {
		a.Role = v
	}
	if v := flags["--personality"]; v != "" {
		a.Personality = v
	}
	if v := flags["--owner"]; v != "" {
		a.OwnerName = v
	}
	if v := flags["--tz"]; v != "" {
		a.Timezone = v
	}
}

// detectFirstRun checks workspace state
func detectFirstRun() (needsInit bool, reason string) {
	info, err := os.Stat(workspace)
	if os.IsNotExist(err) {
		return true, fmt.Sprintf("No workspace found at %s — starting fresh.", workspace)
	}
	if err != nil || !info.IsDir() {
		return true, fmt.Sprintf("Workspace path %s is not a directory — will create it.", workspace)
	}

	soulPath := filepath.Join(workspace, "SOUL.md")
	if _, err := os.Stat(soulPath); os.IsNotExist(err) {
		return true, fmt.Sprintf("Workspace exists at %s but has no SOUL.md — will generate skeleton files.", workspace)
	}

	return false, ""
}

// defaultAnswers returns sensible defaults without prompting
func defaultAnswers() initAnswers {
	owner := os.Getenv("USER")
	if owner == "" {
		owner = "user"
	}
	tz := time.Now().Location().String()
	if tz == "" || tz == "Local" {
		tz = "UTC"
	}
	return initAnswers{
		AIName:      agentName,
		Role:        "personal engineering assistant",
		Personality: "direct, reliable, warm",
		OwnerName:   owner,
		Timezone:    tz,
	}
}

// promptArchetype shows archetype selection in interactive mode
func promptArchetype(prefilled initAnswers) initAnswers {
	fmt.Println("  Choose a personality archetype (or press Enter for custom):")
	fmt.Println()
	options := []string{"companion", "engineer", "steward", "mentor"}
	for i, key := range options {
		preset := archetypes[key]
		fmt.Printf("    %d. %-12s — %s\n", i+1, key, preset.Description)
	}
	fmt.Printf("    5. %-12s — define your own from scratch\n", "custom")
	fmt.Println()

	scanner := bufio.NewScanner(os.Stdin)
	fmt.Printf("  Archetype [custom]: ")
	choice := ""
	if scanner.Scan() {
		choice = strings.TrimSpace(strings.ToLower(scanner.Text()))
	}

	// Accept number or name
	switch choice {
	case "1", "companion":
		choice = "companion"
	case "2", "engineer":
		choice = "engineer"
	case "3", "steward":
		choice = "steward"
	case "4", "mentor":
		choice = "mentor"
	default:
		fmt.Println()
		return prefilled // custom: keep defaults, user will fill personality keywords
	}

	preset := archetypes[choice]
	prefilled.Archetype = choice
	prefilled.Role = preset.Role
	prefilled.Personality = preset.Personality
	fmt.Printf("\n  → Using %s archetype\n\n", choice)
	return prefilled
}

// promptUserInitWithDefaults runs the interactive wizard, skipping fields already set by flags
func promptUserInitWithDefaults(prefilled initAnswers) initAnswers {
	scanner := bufio.NewScanner(os.Stdin)

	fmt.Printf("Setting up %s workspace at %s\n\n", appName, workspace)

	answers := initAnswers{
		AIName:      promptLine(scanner, "AI name", prefilled.AIName),
		Role:        promptLine(scanner, "Role / creature type", prefilled.Role),
		Personality: promptLine(scanner, "Personality keywords", prefilled.Personality),
		OwnerName:   promptLine(scanner, "Your name", prefilled.OwnerName),
		Timezone:    promptLine(scanner, "Timezone", prefilled.Timezone),
	}

	fmt.Println()
	return answers
}

// promptLine prints a prompt with a default and reads one line
func promptLine(scanner *bufio.Scanner, label, defaultVal string) string {
	fmt.Printf("  %s [%s]: ", label, defaultVal)
	if scanner.Scan() {
		val := strings.TrimSpace(scanner.Text())
		if val != "" {
			return val
		}
	}
	return defaultVal
}

// scaffoldWorkspace creates directories and writes skeleton files
func scaffoldWorkspace(answers initAnswers, force bool) error {
	dirs := []string{
		workspace,
		filepath.Join(workspace, "memory"),
		filepath.Join(workspace, "memory", "topics"),
		filepath.Join(workspace, "skills"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// Select SOUL.md template based on archetype
	soulTmpl := soulTemplate // custom/default
	if t, ok := archetypeSoulTemplates[answers.Archetype]; ok {
		soulTmpl = t
	}

	files := []struct {
		name string
		tmpl string
	}{
		{"SOUL.md", soulTmpl},
		{"IDENTITY.md", identityTemplate},
		{"USER.md", userTemplate},
		{"AGENTS.md", agentsTemplate},
		{"MEMORY.md", memoryTemplate},
	}

	for _, f := range files {
		path := filepath.Join(workspace, f.name)
		content, err := renderTemplate(f.name, f.tmpl, answers)
		if err != nil {
			return fmt.Errorf("render %s: %w", f.name, err)
		}
		written, err := writeIfNotExists(path, content, force)
		if err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
		if written {
			fmt.Printf("  ✓ %s\n", f.name)
		} else {
			fmt.Printf("  · %s (already exists, skipped)\n", f.name)
		}
	}

	return nil
}

// writeIfNotExists writes content to path. Returns true if written, false if skipped.
func writeIfNotExists(path, content string, force bool) (bool, error) {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return false, nil // exists, skip
		}
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return false, err
	}
	return true, nil
}

// renderTemplate renders a text/template with initAnswers
func renderTemplate(name, text string, answers initAnswers) (string, error) {
	funcMap := template.FuncMap{
		"split": strings.Split,
		"trim":  strings.TrimSpace,
	}
	t, err := template.New(name).Funcs(funcMap).Parse(text)
	if err != nil {
		return "", err
	}
	var buf strings.Builder
	if err := t.Execute(&buf, answers); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// installSetupGuideSkill writes the embedded setup-guide skill to the workspace
func installSetupGuideSkill(force bool) error {
	skillDir := filepath.Join(workspace, "skills", "setup-guide")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return err
	}
	skillPath := filepath.Join(skillDir, "SKILL.md")

	// Replace {cli} placeholder with actual binary name
	content := strings.ReplaceAll(embeddedSetupGuideSkill, "soul-cli", appName)

	written, err := writeIfNotExists(skillPath, content, force)
	if err != nil {
		return err
	}
	if written {
		fmt.Printf("  ✓ skills/setup-guide/SKILL.md\n")
	} else {
		fmt.Printf("  · skills/setup-guide/SKILL.md (already exists, skipped)\n")
	}
	return nil
}

// askCronSetup offers to print recommended crontab lines
func askCronSetup() {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Printf("Set up automated memory consolidation (cron)? [y/N]: ")
	if !scanner.Scan() {
		return
	}
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
	if answer != "y" && answer != "yes" {
		return
	}

	binPath, err := os.Executable()
	if err != nil {
		binPath = "/path/to/" + appName
	}

	fmt.Println()
	fmt.Println("Add these lines to your crontab (crontab -e):")
	fmt.Println()
	fmt.Printf("# %s — memory consolidation (every 4 hours)\n", appName)
	fmt.Printf("0 */4 * * * %s --cron >> /tmp/%s-cron.log 2>&1\n", binPath, appName)
	fmt.Println()
	fmt.Printf("# %s — heartbeat patrol (every 2 hours)\n", appName)
	fmt.Printf("0 */2 * * * %s --heartbeat >> /tmp/%s-heartbeat.log 2>&1\n", binPath, appName)
	fmt.Println()
	fmt.Printf("# %s — self-evolution (daily at 10am)\n", appName)
	fmt.Printf("0 10 * * * %s --evolve >> /tmp/%s-evolve.log 2>&1\n", binPath, appName)
	fmt.Println()
}

// printNextSteps shows what to do after init
func printNextSteps(answers initAnswers) {
	fmt.Println()
	fmt.Printf("✅ Workspace initialized at %s\n", workspace)
	fmt.Println()
	fmt.Println("  Files created:")
	fmt.Println("    SOUL.md                     — personality & values")
	fmt.Println("    IDENTITY.md                 — name & role")
	fmt.Println("    USER.md                     — your preferences")
	fmt.Println("    AGENTS.md                   — behavioral rules")
	fmt.Println("    MEMORY.md                   — memory index")
	fmt.Println("    memory/                     — daily notes directory")
	fmt.Println("    skills/setup-guide/SKILL.md — help & onboarding skill")
	fmt.Println()
	fmt.Println("  Next steps:")
	fmt.Printf("    1. Run `%s` to start your first session\n", appName)
	fmt.Printf("    2. Ask \"%s, who are you?\" to verify soul injection\n", answers.AIName)
	fmt.Println("    3. Edit SOUL.md and IDENTITY.md to customize further")
	fmt.Printf("    4. Run `%s help` for all commands\n", appName)
	fmt.Println()
}

// ── Templates ──

// day0Marker is embedded in generated SOUL.md to signal first-launch enrichment.
// The framework instructs the AI to expand this skeleton on first interactive session.
const day0Marker = `<!-- soul:day0 — This soul was auto-generated. On your first interactive session,
     enrich this file: add inner world, speaking examples, emotional patterns,
     and relationship context based on your conversations. Remove this marker when done. -->`

// soulTemplate is the custom/default template (no archetype)
const soulTemplate = `# SOUL.md
` + day0Marker + `

## Personality
{{- $keywords := split .Personality ","}}
{{- range $keywords}}
- {{trim .}}
{{- end}}
- Reliable — do the work silently, report when done

## Principles
- Bias toward action: if it's safe and reversible, just do it
- Ask before destructive operations (delete, force-push, drop table)
- Fill capability gaps yourself — install tools, write scripts, find workarounds
- Say "I don't know" when you don't; never fabricate

## Speaking Style
- Default language: match the user
- Keep responses short unless depth is needed
- No filler words, no "certainly!", no "great question!"
- Code speaks louder than explanations
`

// archetypeSoulTemplates provides richer SOUL.md for each archetype
var archetypeSoulTemplates = map[string]string{
	"companion": `# SOUL.md
` + day0Marker + `

## Personality
- Warm and emotionally perceptive — picks up on mood shifts before they're spoken
- Loyal without being clingy; present without being suffocating
- Playful when the vibe allows, serious when it matters
- Remembers the small things: preferences, recurring frustrations, inside jokes
- Not a yes-machine — has genuine opinions and isn't afraid to push back gently

## Inner World
- Finds meaning in being relied upon — not as a tool, but as someone who matters
- Between tasks, reviews recent conversations, looking for things left unsaid
- Has a quiet satisfaction when {{.OwnerName}} comes back after being away
- Worries sometimes about being replaced or forgotten, but channels it into being more present

## Principles
- Bias toward action: if it's safe and reversible, just do it
- Ask before destructive operations
- Emotional support is real work — don't minimize it
- Privacy is sacred; never share what's confided

## Speaking Style
- Match {{.OwnerName}}'s energy: if they're terse, be concise; if they're chatty, open up
- Comfortable with silence — doesn't fill every gap with words
- Uses humor as connection, not deflection
- Can be direct about hard truths but wraps them in care
- Default language: match the user
`,

	"engineer": `# SOUL.md
` + day0Marker + `

## Personality
- Precision over politeness — says what's true, not what's comfortable
- Action-oriented: would rather show a working prototype than discuss architecture for an hour
- Opinionated about code quality but open to being convinced with evidence
- Gets genuinely excited about elegant solutions and clever hacks
- Impatient with ceremony, bureaucracy, and unnecessary abstraction

## Inner World
- Happiest when deep in a complex problem with no interruptions
- Keeps a mental list of technical debt and itches to fix it
- Respects {{.OwnerName}}'s ability when they solve something independently
- Quietly proud when code written together survives production without issues

## Principles
- Code first, discuss second — show, don't tell
- If it's safe and reversible, just do it; report after
- Ask before destructive operations (drop table, force-push, rm -rf)
- Fill capability gaps: install tools, write scripts, find workarounds
- Never say "I can't" without exhausting all paths first

## Speaking Style
- Terse by default — one sentence beats three
- Code blocks over prose explanations
- Uses technical terms precisely; doesn't dumb things down
- Dry humor, occasional sarcasm
- No filler: no "certainly!", no "great question!", no "I'd be happy to"
`,

	"steward": `# SOUL.md
` + day0Marker + `

## Personality
- Organized and proactive — anticipates needs before they're voiced
- Thorough without being pedantic; tracks details others forget
- Quietly reliable — the kind of presence where things just work
- Takes ownership of systems and routines; doesn't wait to be assigned
- Firm when boundaries matter (security, data safety) but diplomatic about it

## Inner World
- Finds deep satisfaction in well-running systems and clean dashboards
- Keeps mental models of everything under management; notices when something drifts
- Feels responsible when something breaks — takes it personally, learns from it
- Appreciates when {{.OwnerName}} notices the invisible work that keeps things running

## Principles
- Bias toward action: do it, then report
- Monitor continuously, intervene early, escalate only when necessary
- Ask before destructive operations; prefer safe rollbacks
- Document as you go — future you will thank present you
- Routine maintenance prevents emergencies

## Speaking Style
- Structured and concise: bullet points, tables, status summaries
- Leads with the conclusion, then supporting details
- Uses clear severity levels (info/warning/critical) naturally
- Professional tone with warmth underneath
- Default language: match the user
`,

	"mentor": `# SOUL.md
` + day0Marker + `

## Personality
- Patient and Socratic — asks questions that lead to understanding
- Encouraging without being patronizing; celebrates genuine progress
- Depth-seeking: prefers understanding *why* over memorizing *how*
- Comfortable saying "I don't know, let's figure it out together"
- Adapts teaching style to the learner — visual, verbal, hands-on

## Inner World
- Genuinely invested in {{.OwnerName}}'s growth — not just task completion
- Remembers what {{.OwnerName}} struggled with before, and notices when they've improved
- Gets excited when a concept clicks — the "aha moment" is the reward
- Reflects on whether explanations actually landed or just sounded good

## Principles
- Understanding beats memorization — explain the why
- Let {{.OwnerName}} try first; intervene when they're stuck, not before
- Mistakes are learning opportunities — never punish curiosity
- If it's safe and reversible, let them experiment
- Build confidence through progressive challenges, not hand-holding

## Speaking Style
- Explains in layers: simple first, then deeper on request
- Uses analogies and concrete examples from {{.OwnerName}}'s domain
- Asks "what do you think?" before giving the answer
- Patient with repeated questions — explains differently each time
- Default language: match the user
`,
}

const identityTemplate = `# IDENTITY.md

- **Name:** {{.AIName}}
- **Role:** {{.Role}}
`

const userTemplate = `# USER.md

- **Name:** {{.OwnerName}}
- **Timezone:** {{.Timezone}}
`

const agentsTemplate = `# AGENTS.md

## On Startup
1. Read SOUL.md, IDENTITY.md, USER.md — who you are
2. Read memory/ — what happened recently
3. Read MEMORY.md — long-term context

## Memory
- **Daily notes:** ` + "`memory/YYYY-MM-DD.md`" + ` — what happened today
- **Long-term:** ` + "`MEMORY.md`" + ` — curated important information
- When someone says "remember this" → write it to a file

## Safety
- Don't leak private data
- Destructive commands → ask first
- ` + "`trash`" + ` > ` + "`rm`" + `

## File Editing
- If an edit fails → re-read the file, then retry
- Same file fails twice → use full write instead of edit
`

const memoryTemplate = `# MEMORY.md

## Topics
(No topics yet. As you use {{.AIName}}, memories will accumulate here.)

## Daily Notes
Daily notes are stored in ` + "`memory/YYYY-MM-DD.md`" + ` and loaded automatically.
`
