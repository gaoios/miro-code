# CLI Reference

## Synopsis

```
miro [flags]
miro [command] [args]
```

## Modes

### Interactive (default)

```bash
miro
```

Launches Claude Code with soul prompt injected. The process replaces itself (`syscall.Exec`) — Claude gets your full terminal.

### One-Shot

```bash
miro -p "check disk usage and warn if above 80%"
```

Runs a single task with soul context, then exits.

### Resume

```bash
miro -r                 # TUI picker for recent sessions
miro -r abc123          # Resume specific session by ID
miro -r --chrome        # Resume with Chrome automation enabled
```

### Cron

```bash
miro --cron
```

Memory consolidation: scan recent sessions, update daily notes, extract patterns. See [Automation Guide](../guides/automation.md).

### Heartbeat

```bash
miro --heartbeat
```

Health check: monitor services, process tasks, detect anomalies. See [Automation Guide](../guides/automation.md).

### Evolve

```bash
miro --evolve
```

Self-improvement: review interactions, update soul files, fix bugs. See [Automation Guide](../guides/automation.md).

### Server

```bash
miro server [--host HOST] [--port PORT] [--token TOKEN]
```

Start the HTTP server with Web UI. See [Server Mode Guide](../guides/server.md).

### Spawn (multi-CLI dispatch)

```bash
miro spawn --bare --backend <name> --model <native-model-id> --project <absolute-path> "<task>" [--wait]
```

Requires a running server (`miro server`). `--bare` and `--model` are mandatory.

| Backend | Example model ID |
|---------|------------------|
| `codex` | `gpt-6-astra` |
| `grok` | `grok-4.6` |
| `kimi` | `kimi-code/k3` |
| `agy` | `gemini-3.8-flash-low`, `gemini-3.1-pro-low` |
| `opencode` | `opencode-go/glm-5.3-flash` (any provider/model pair from its `model_map`) |

Model IDs are native to each CLI; use each CLI's own `models` command for the current list. See [Multi-CLI Agent Workspace](../guides/multi-cli-workspace.md).

## Commands

### `init`

First-run setup wizard — creates workspace, generates soul files, installs setup-guide skill.

```bash
miro init                          # interactive wizard with archetype selection
miro init --yes                    # use all defaults, no prompts
miro init --archetype companion    # use companion personality template
miro init --archetype engineer --name kuro --owner alex --tz America/New_York
miro init --force                  # overwrite existing files
```

| Flag | Description |
|------|-------------|
| `--archetype` | Personality template: `companion`, `engineer`, `steward`, `mentor` |
| `--name` | AI name (default: binary name) |
| `--role` | Role description |
| `--personality` | Comma-separated keywords |
| `--owner` | Owner's name (default: `$USER`) |
| `--tz` | Timezone (default: system timezone) |
| `--yes`, `-y` | Skip prompts, use defaults |
| `--force`, `-f` | Overwrite existing files |

Generated files include a `<!-- soul:day0 -->` marker that triggers automatic personality enrichment on first interactive session.

### `status`

Quick health check (doesn't launch Claude):

```bash
miro status
```

Shows: service health, Claude Code version, database stats, config validity.

### `doctor`

Deep diagnostics:

```bash
miro doctor
```

Shows: everything in `status` plus process list, disk usage, model endpoint checks, metrics anomalies, memory analysis.

### `config`

Display current configuration:

```bash
miro config
```

### `prompt`

Print the assembled soul prompt with per-section token stats:

```bash
miro prompt
```

### `log`

View daily notes:

```bash
miro log          # today's notes
miro log 1        # yesterday's notes
miro log 3        # 3 days ago
```

### `diff`

Show soul/memory changes since last commit:

```bash
miro diff
```

### `clean`

Clean up old temporary directories:

```bash
miro clean
```

### `lint`

Validate markdown file formats:

```bash
miro lint
```

### `notify`

Send a Telegram message:

```bash
miro notify "deployment complete"
miro notify-photo https://example.com/screenshot.png "Dashboard screenshot"
```

### `build`

Safe self-compilation with automatic rollback:

```bash
miro build
```

Steps: backup current binary → compile → run tests → deploy. Rolls back on failure.

### `update`

Pull latest source and rebuild:

```bash
miro update
```

Equivalent to `git pull && miro build`.

### `versions`

List saved binary versions:

```bash
miro versions
```

### `rollback`

Restore a previous binary version:

```bash
miro rollback       # rollback to previous version
miro rollback 2     # rollback 2 versions back
```

### `sessions` / `ss`

Interactive session browser (TUI):

```bash
miro sessions            # browse all sessions
miro ss                  # alias
miro ss kubernetes       # pre-filter by keyword
miro ss --chrome         # show chrome-enabled sessions
```

### `db`

Session database management:

```bash
miro db stats            # session counts
miro db search <keyword> # search session summaries
miro db pending          # sessions needing review
miro db gc               # clean up deleted sessions
miro db patterns         # list extracted patterns
miro db cultivate        # generate skills from mature patterns
miro db recall           # sessions pending summary import
miro db save-batch       # batch import pending summaries
```

## Flags

### Global Flags

| Flag | Description |
|------|-------------|
| `-p "task"` | One-shot mode: run task and exit |
| `-r [id]` | Resume a previous session |
| `--cron` | Run memory consolidation |
| `--heartbeat` | Run health check |
| `--evolve` | Run self-improvement |
| `--chrome` | Enable Chrome automation (passed to Claude Code) |

### Init Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--archetype` | *(custom)* | Personality template: companion, engineer, steward, mentor |
| `--name` | binary name | AI name |
| `--role` | "personal engineering assistant" | Role description |
| `--personality` | "direct, reliable, warm" | Comma-separated keywords |
| `--owner` | `$USER` | Owner name |
| `--tz` | system timezone | Timezone string |
| `--yes` / `-y` | — | Skip interactive prompts |
| `--force` / `-f` | — | Overwrite existing files |

### Passthrough Flags

Any flag soul-cli doesn't recognize is forwarded to Claude Code:

```bash
miro --chrome                    # forwarded: claude --chrome
miro -p "task" --verbose         # forwarded: claude --verbose
miro --model claude-sonnet-4-20250514   # forwarded: claude --model ...
```

### Server Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--host` | `127.0.0.1` | Bind address |
| `--port` | `9847` | Listen port |
| `--token` | — | Auth token (required) |
