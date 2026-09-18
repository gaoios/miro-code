# FAQ

## General

### What is soul-cli?

A Go CLI that wraps [Claude Code](https://docs.anthropic.com/en/docs/claude-code) with persistent identity, memory, and self-evolution. It assembles markdown files into a system prompt so your AI remembers who it is across sessions.

### Does it work without OpenClaw?

Yes. OpenClaw integration is optional. Without it, configure everything via `config.json` and environment variables.

### What models does it support?

soul-cli launches Claude Code, which supports all Claude models. The soul prompt is model-agnostic — it works with Opus, Sonnet, and Haiku.

### How much does it cost?

soul-cli itself is free and open source (MIT). You need a Claude Code subscription or API access. The soul prompt typically adds 10-30k tokens of context per session.

## Installation

### "claude: command not found"

Claude Code must be installed and in your PATH:

```bash
# Install Claude Code
npm install -g @anthropic-ai/claude-code

# Verify
claude --version
```

For cron jobs, make sure PATH includes the Claude binary location:

```crontab
0 */4 * * * PATH="$HOME/.local/bin:$HOME/go/bin:$PATH" miro --cron
```

### Can I install without Go?

Not currently. soul-cli is distributed as source. You need Go 1.21+ to build it. Pre-built binaries may be available in the future.

### How do I update?

```bash
cd /path/to/soul-cli
miro update    # git pull + safe build with rollback
```

Or manually:

```bash
git pull
miro build     # safe compile with backup
```

## Soul Files

### Do I need all the soul files?

No. Only `SOUL.md` and `IDENTITY.md` are required. Everything else is optional and additive.

### Can the AI modify its own soul files?

Yes, during `--evolve` mode. The AI reviews recent interactions and makes small adjustments. Use `CORE.md` to protect rules that must never change.

### My prompt is too large

Run `miro prompt` to see per-section token usage. Common culprits:

- `TOOLS.md` with too many credentials → move rarely-used ones to memory topics
- Daily notes that are too verbose → the cron mode will trim them
- Too many project `CLAUDE.md` files → reduce `projectRoots` scope

The soft budget is 100k tokens. A typical setup uses 15-25k.

### Can I use non-English soul files?

Yes. Write in any language. The AI will respond in the language of your soul files by default (or you can specify a language preference in SOUL.md).

## Memory

### How do daily notes get created?

Two ways:

1. **Manually** — Write `memory/YYYY-MM-DD.md` yourself
2. **Automatically** — Run `miro --cron`, which scans recent Claude Code sessions and generates summaries

### My memory is getting too big

- Run `miro db gc` to clean up deleted sessions
- Run `miro clean` to remove old temp directories
- Promote important daily notes to topic files, then delete old daily notes
- Keep `MEMORY.md` under 200 lines

### How do I search past conversations?

```bash
miro db search "kubernetes"    # search session summaries
miro ss kubernetes             # interactive TUI browser
```

## Server Mode

### Can I expose the server to the internet?

Yes, but put it behind a reverse proxy with TLS. See the [Server Mode Guide](guides/server.md#reverse-proxy) for nginx configuration.

### Sessions keep dying

Check:

1. `idleTimeoutMin` — increase if sessions idle between messages
2. `maxLifetimeHours` — increase for long-running tasks
3. System resources — each session runs a Claude Code process

### Can multiple users share the server?

The current auth model is a single shared token. All users with the token can see and interact with all sessions. Multi-user auth is not yet implemented.

## Automation

### My cron jobs aren't running

Common issues:

1. **PATH** — cron runs with minimal PATH. Prefix with the full path to both `claude` and your binary
2. **Lock file** — check if `/tmp/<appname>.lock` exists from a crashed run. Delete it if stale
3. **Permissions** — ensure the binary and workspace are readable by the cron user

### How do I know if evolve changed something?

- Check `miro diff` for uncommitted changes
- Check today's daily notes (`miro log`) — evolve records what it changed
- If Telegram is configured, evolve sends a report

## Troubleshooting

### "lock file exists, another instance is running"

A previous run crashed without cleaning up. Check if another instance is actually running:

```bash
ps aux | grep miro
```

If nothing is running, remove the stale lock:

```bash
rm /tmp/miro.lock    # or rmdir /tmp/miro.lock.d for NFS
```

### "token budget exceeded"

Your assembled prompt is too large. Run `miro prompt` to identify the largest sections and trim them. See [My prompt is too large](#my-prompt-is-too-large) above.
