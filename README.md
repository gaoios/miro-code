# miro-code

A soul-aware launcher for [Claude Code](https://docs.anthropic.com/en/docs/claude-code). It gives your AI a persistent identity, memory, and the ability to evolve across sessions — plus built-in multi-CLI orchestration: dispatch coding tasks to other logged-in coding CLIs with one command.

Claude Code starts fresh every time — no memory, no personality, no idea who you are. **miro-code** fixes this by assembling markdown files (identity, personality, memory, skills) into a system prompt injected at launch.

> 📖 **使用教程（中文）**：[docs/miro/tutorial.zh.md](docs/miro/tutorial.zh.md) · GitBook 版：https://xlegao.gitbook.io/mirobiz/
>
> Forked & extended from [kiyor/soul-cli](https://github.com/kiyor/soul-cli) — upstream project by @kiyor.

## Quick Start

```bash
git clone https://github.com/gaoios/miro-code.git && cd miro-code
go build -o miro .
mv miro ~/go/bin/

miro init                          # interactive wizard
miro init --archetype companion    # pick a personality archetype
miro                               # Claude Code, but it remembers
```

**Dispatch a task to another coding CLI** (requires `miro server` running + the target CLI logged in):

```bash
miro spawn --bare --backend grok --model grok-4.6 \
  --project /absolute/path/to/repo \
  "Review the concurrency logic in src/ and list the 3 riskiest issues" --wait
```

The `init` command creates your workspace, generates soul files, and installs a setup-guide skill — no manual file editing needed.

### Personality Archetypes

| Archetype | Vibe |
|-----------|------|
| `companion` | Emotionally present partner — remembers the small things, picks up on mood |
| `engineer` | Technical peer — code first, explain later, dry humor |
| `steward` | Operations manager — proactive, organized, quietly reliable |
| `mentor` | Patient teacher — Socratic questions, layered explanations |
| *(custom)* | Define your own from keywords |

On first launch, the AI automatically enriches its personality based on your conversation (day-0 self-enrichment).

### AI-Friendly (No Stdin Required)

```bash
miro init --archetype engineer --name kuro --owner alex --tz America/Los_Angeles
```

All flags provided = zero interactive prompts. Perfect for scripting or AI-driven setup.

> **Requires:** Go 1.25+ and [Claude Code](https://docs.anthropic.com/en/docs/claude-code)

### One-Command Linux Deploy

Don't want to set up manually? Feed the bootstrap guide to Claude Code and let it do everything:

```bash
export CLAUDE_CODE_OAUTH_TOKEN="<YOUR_ANTHROPIC_TOKEN>"  # your OAuth token first
claude -p "$(curl -sfL https://raw.githubusercontent.com/kiyor/soul-cli/main/bootstrap.md)" --dangerously-skip-permissions
```

It'll ask you a few questions (AI name, personality, timezone), then handle Go, Node.js, build, systemd — everything. It even scans your existing Claude Code sessions to personalize the soul.

See [Linux Server Deployment Guide](docs/guides/linux-deploy.md) if you prefer doing it yourself.

## How It Works

```
Soul Files + Memory + Skills  →  soul-cli  →  Claude Code (with soul)
     (markdown)                  (assembles)    (--append-system-prompt)
```

- **`SOUL.md`** — personality, values, speaking style
- **`USER.md`** — your timezone, preferences, expertise
- **`MEMORY.md`** + daily notes — what happened yesterday, long-term knowledge
- **`AGENTS.md`** — behavioral rules and guardrails

The binary name determines identity: build as `miro`, `jarvis`, or `atlas` — all paths, env vars, and logs derive from it.

## Features

| Feature | How |
|---------|-----|
| **Memory** | Daily notes auto-generated from sessions, long-term topics, SQLite session DB |
| **Evolution** | `--evolve` cron reviews interactions and self-adjusts soul files |
| **Server mode** | Built-in HTTP server + Web UI for persistent sessions |
| **Automation** | `--cron` (memory), `--heartbeat` (health checks), `--evolve` (self-improvement) |
| **Multi-agent** | One codebase, multiple binaries with isolated data |
| **Multi-CLI workspace** | Claude supervisor + Codex, Grok Build, Kimi, and Antigravity workers; shared tool CLIs such as Meoo |
| **Safety** | Symlink rejection, secret leak detection, CORE.md protection, auto-rollback |
| **Telegram** | Notifications, reports, conversation context injection |

## Usage

```bash
miro                         # interactive session
miro -p "check disk usage"   # one-shot task
miro -r                      # resume previous session
miro server --token secret   # HTTP server + Web UI
miro --cron                  # memory consolidation
miro --heartbeat             # health check patrol
miro --evolve                # self-improvement
miro status                  # quick diagnostics
```

### Multi-CLI Agent Workspace

Server mode can run multiple AI CLI backends behind the same session API and
Web UI. Claude remains the default supervisor, while enabled workers receive
bounded tasks through `spawn --bare`:

```bash
miro server
miro spawn --bare --backend codex --model gpt-6-astra --project "$PWD" "Implement and test the change" --wait
miro spawn --bare --backend grok --project "$PWD" "Review the implementation independently" --wait
```

Non-conversational CLIs are registered separately under `tools`. They run in
the same project environment but do not pretend to be model backends. This is
the extension point for deployment, image generation, and platform CLIs such
as Meoo or a locally installed Jimeng-compatible CLI.

See [Multi-CLI Agent Workspace](docs/guides/multi-cli-workspace.md).

## Documentation

**[Full documentation →](https://kiyor.github.io/soul-cli/)**

- [Getting Started](https://kiyor.github.io/soul-cli/getting-started/) — install, configure, launch
- [Core Concepts](https://kiyor.github.io/soul-cli/concepts/) — soul files, memory, evolution
- [Soul Files Guide](https://kiyor.github.io/soul-cli/guides/soul-files/) — writing each markdown file
- [Server Mode](https://kiyor.github.io/soul-cli/guides/server/) — HTTP API, Web UI, deployment
- [Automation](https://kiyor.github.io/soul-cli/guides/automation/) — cron, heartbeat, evolve
- [CLI Reference](https://kiyor.github.io/soul-cli/reference/cli/) — every command and flag
- [API Reference](https://kiyor.github.io/soul-cli/reference/api/) — server endpoints
- [FAQ](https://kiyor.github.io/soul-cli/faq/)

## License

MIT
