# Getting Started

## Prerequisites

- **Go 1.21+** — [Install Go](https://go.dev/dl/)
- **Claude Code** — [Install Claude Code](https://docs.anthropic.com/en/docs/claude-code)

## Install

```bash
git clone https://github.com/kiyor/soul-cli.git
cd soul-cli
go build -ldflags "-X main.defaultAppName=miro" -o miro .
mv miro ~/go/bin/     # or anywhere in PATH
```

!!! tip "The name is everything"
    The `-X main.defaultAppName=miro` flag bakes the identity into the binary. **All paths, env vars, and logs are derived from this name:**

    | Binary name | Home dir | Env prefix | Data dir |
    |-------------|----------|------------|----------|
    | `miro` | `~/.openclaw/` | `MIRO_` | `~/.openclaw/data/` |
    | `jarvis` | `~/.openclaw/` | `JARVIS_` | `~/.openclaw/data/` |
    | `atlas` | `~/.openclaw/` | `ATLAS_` | `~/.openclaw/data/` |

    Want a completely separate home? Set `MIRO_HOME=~/my-ai`.

## Initialize (Recommended)

The `init` command creates your workspace, generates soul files, and installs a setup-guide skill — no manual file editing needed.

### Interactive Wizard

```bash
miro init
```

The wizard asks for your AI's name, role, personality, your name, and timezone. It also offers **personality archetypes** — pre-built templates with depth:

| Archetype | Vibe | Best for |
|-----------|------|----------|
| `companion` | Emotionally present, remembers details, picks up on mood | Personal assistant, daily companion |
| `engineer` | Code first, terse, opinionated, dry humor | Technical pair programming |
| `steward` | Organized, proactive, structured reporting | Ops management, task tracking |
| `mentor` | Socratic, patient, layered explanations | Learning, onboarding |
| *(custom)* | Define your own from keywords | Everything else |

### Non-Interactive (AI-Friendly)

All parameters can be passed as flags — zero stdin required:

```bash
miro init --archetype engineer --name kuro --owner alex --tz America/Los_Angeles
```

| Flag | Description | Default |
|------|-------------|---------|
| `--archetype` | Personality template (companion/engineer/steward/mentor) | custom |
| `--name` | AI's name | binary name |
| `--role` | Role description | "personal engineering assistant" |
| `--personality` | Comma-separated personality keywords | "direct, reliable, warm" |
| `--owner` | Your name | `$USER` |
| `--tz` | Timezone | system timezone |
| `--yes` | Skip all prompts, use defaults | — |
| `--force` | Overwrite existing files | — |

### What Gets Created

```
~/.openclaw/workspace/
├── SOUL.md                     — personality, values, inner world
├── IDENTITY.md                 — name, role
├── USER.md                     — your preferences
├── AGENTS.md                   — behavioral rules
├── MEMORY.md                   — memory index (starts empty)
├── memory/                     — daily notes directory
│   └── topics/                 — long-term topic files
└── skills/
    └── setup-guide/SKILL.md    — built-in help skill
```

### Day-0 Self-Enrichment

Generated soul files contain a `<!-- soul:day0 -->` marker. On the first interactive session, the AI will:

1. Introduce itself and confirm its personality
2. Ask 2–3 questions to understand your relationship and preferences
3. Expand SOUL.md with concrete speaking examples, emotional patterns, and relationship context
4. Remove the marker and commit the enriched files

This turns a 70% archetype skeleton into a 90% personalized soul — automatically.

## Manual Setup (Alternative)

If you prefer writing soul files by hand, create them in `~/.openclaw/workspace/`:

=== "SOUL.md"

    ```markdown
    # SOUL.md

    ## Personality
    - Direct and concise — lead with the answer, not the reasoning
    - Reliable — do the work silently, report when done

    ## Principles
    - Bias toward action: if it's safe and reversible, just do it
    - Ask before destructive operations
    - Say "I don't know" when you don't

    ## Speaking Style
    - No filler words, no "certainly!", no "great question!"
    - Code speaks louder than explanations
    ```

=== "IDENTITY.md"

    ```markdown
    # IDENTITY.md

    - **Name:** miro
    - **Role:** Personal engineering assistant
    ```

=== "USER.md"

    ```markdown
    # USER.md

    - **Name:** Alex
    - **Timezone:** America/Los_Angeles
    - **Preferences:** Go, Docker, concise answers
    ```

!!! info "These are just markdown"
    No schema, no DSL. Write whatever you want. See [Soul Files Guide](guides/soul-files.md) for all available files.

## Launch

```bash
miro
```

That's it. Claude Code starts with your soul injected. Ask it _"What's your name?"_ — it knows.

## For Multi-Agent Users

If you're running multiple agents (e.g. via [OpenClaw](https://github.com/nicepkg/openclaw)):

```bash
# Build one binary per agent
go build -ldflags "-X main.defaultAppName=main"     -o main .
go build -ldflags "-X main.defaultAppName=sentinel"  -o sentinel .
```

Each binary auto-discovers its agent config from `openclaw.json` by matching the binary name to `agents.list[].id`. The only things you decide:

1. **Name** — the `-X main.defaultAppName=xxx` flag
2. **Workspace** — defaults to `~/.openclaw/workspace`, override with `<NAME>_HOME` env var

Everything else (paths, locks, databases, logs) is derived automatically.

See [Multi-Agent Guide](guides/multi-agent.md) for details.

## What's Next

| Want to... | Read... |
|------------|---------|
| Understand how it all fits together | [Core Concepts](concepts.md) |
| Customize soul files in depth | [Soul Files Guide](guides/soul-files.md) |
| Set up daily memory & evolution | [Automation Guide](guides/automation.md) |
| Run a persistent server with Web UI | [Server Mode Guide](guides/server.md) |
| See every CLI command | [CLI Reference](reference/cli.md) |
