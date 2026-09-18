---
name: setup-guide
description: |
  Comprehensive guide to soul-cli setup, configuration, and troubleshooting.
  Use when the user asks about soul files, memory system, skills, automation,
  server mode, multi-agent setup, or how soul-cli works.
triggers:
  - how do I
  - what is soul-cli
  - setup
  - getting started
  - how does memory work
  - what are soul files
---

# Setup Guide

## What is soul-cli?

soul-cli wraps Claude Code with persistent identity files (soul files), memory, and automation.
You write markdown files that define who the AI is, and soul-cli assembles them into a system prompt
every time it launches.

## Soul Files

| File | Purpose | Required |
|------|---------|----------|
| `SOUL.md` | Personality, values, speaking style | Recommended |
| `IDENTITY.md` | Name, role, vibe — the public-facing identity | Recommended |
| `USER.md` | Owner's name, timezone, preferences | Recommended |
| `AGENTS.md` | Behavioral rules, memory instructions, safety | Optional |
| `TOOLS.md` | External tools, APIs, credentials reference | Optional |
| `MEMORY.md` | Long-term memory index (links to topic files) | Optional |
| `BOOT.md` | Startup protocol override (advanced) | Optional |
| `CORE.md` | Read-only rules the AI cannot modify (owner-controlled) | Optional |

All files live in the workspace directory (default: `~/.openclaw/workspace`).
Missing files are silently skipped — start with just SOUL.md + IDENTITY.md.

## Memory System

Three tiers:

1. **Daily notes** (`memory/YYYY-MM-DD.md`) — auto-loaded for today and yesterday.
   Write observations, session summaries, events here.

2. **Topic files** (`memory/topics/*.md`) — long-term knowledge organized by subject.
   Index them in MEMORY.md so the AI knows they exist.

3. **Session DB** (SQLite) — tracks Claude Code session summaries.
   Managed automatically by `--cron` mode.

### Key commands

```
soul-cli db stats          # session count, summary count
soul-cli db search <term>  # search session summaries
soul-cli db recall         # view sessions pending scan
soul-cli log               # view today's daily notes
soul-cli log 1             # view yesterday's daily notes
```

## Common Commands

| Command | What it does |
|---------|-------------|
| `soul-cli` | Start interactive session (default) |
| `soul-cli -p "task"` | One-shot task, exits when done |
| `soul-cli -r` | Resume a previous session (TUI picker) |
| `soul-cli -r <id>` | Resume specific session by ID |
| `soul-cli status` | Quick health check |
| `soul-cli prompt` | Print the full assembled system prompt |
| `soul-cli diff` | Show soul/memory file changes since last commit |
| `soul-cli config` | Show current configuration and paths |
| `soul-cli doctor` | Deep diagnostics |
| `soul-cli lint` | Validate markdown file formats |
| `soul-cli clean` | Clean old session temp directories |

## Skills

Skills are modular capabilities defined as `skills/<name>/SKILL.md` with YAML frontmatter.
The AI sees a skill index table in its prompt and can read the full SKILL.md when needed.

### Creating a skill

```
mkdir -p skills/my-skill
cat > skills/my-skill/SKILL.md << 'EOF'
---
name: my-skill
description: |
  What this skill does and when to trigger it.
---

# My Skill

Instructions for the AI on how to use this skill.
EOF
```

Skills can include shell scripts, config files, or any supporting files alongside SKILL.md.

## Automation

### Cron (memory consolidation)

Scans recent Claude Code sessions, summarizes them, and updates the session database.

```
# Recommended: every 4 hours
0 */4 * * * /path/to/soul-cli --cron >> /tmp/soul-cron.log 2>&1
```

### Heartbeat (health patrol)

Periodic check-in: service health, task status, proactive observations.

```
# Recommended: every 2 hours
0 */2 * * * /path/to/soul-cli --heartbeat >> /tmp/soul-heartbeat.log 2>&1
```

### Evolve (self-improvement)

Reviews recent interactions, updates soul/memory files, fixes issues.

```
# Recommended: once daily
0 10 * * * /path/to/soul-cli --evolve >> /tmp/soul-evolve.log 2>&1
```

## Server Mode

Run as an HTTP API server for persistent sessions (Web UI, external integrations):

```
soul-cli server                      # localhost:9090
soul-cli server --port 8080          # custom port
soul-cli server --host 0.0.0.0      # expose to network
soul-cli server --token SECRET       # require auth token
```

## Multi-Agent

Compile the same source with different names to create independent agents:

```
go build -o alice .    # agent "alice"
go build -o bob .      # agent "bob"
```

Each binary name resolves to its own workspace, config, and identity.
Configure in `openclaw.json` under `agents.list[]`.

## Troubleshooting

**"The AI doesn't know its name / has no personality"**
→ Check that SOUL.md and IDENTITY.md exist in the workspace. Run `soul-cli prompt` to verify they appear in the assembled prompt.

**"Soul files aren't loading"**
→ Run `soul-cli config` to see the resolved workspace path. Ensure files are in that directory.

**"Memory isn't updating"**
→ Check if `--cron` is running in your crontab. Run `soul-cli db stats` to see session counts.

**"Skills don't show up"**
→ Verify the directory structure: `skills/<name>/SKILL.md` must exist with valid YAML frontmatter. Run `soul-cli lint` to check.

**"Server won't start"**
→ Check if the port is already in use. Try a different port with `--port`.
