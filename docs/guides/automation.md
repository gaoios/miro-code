# Automation: Cron, Heartbeat, Evolve

soul-cli has three automated modes that run Claude Code as a background task with specific goals. These are typically scheduled via cron.

## Overview

| Mode | Purpose | Typical Schedule |
|------|---------|-----------------|
| `--cron` | Memory consolidation — scan sessions, update daily notes | Every 4 hours |
| `--heartbeat` | Health checks — monitor services, process tasks | Every 2 hours |
| `--evolve` | Self-improvement — review and update soul files | Daily at 10am |

All three modes:

1. Run Claude Code as a **subprocess** (not interactive)
2. Inject a task-specific prompt on top of the soul prompt
3. Execute **post-hooks** after Claude exits
4. Optionally send a **Telegram report**

## Cron Mode

```bash
miro --cron
```

### What it does

1. Scans recent Claude Code session JSONL files
2. Summarizes unseen sessions
3. Updates today's daily notes (`memory/YYYY-MM-DD.md`)
4. Extracts behavioral patterns from sessions
5. Imports summaries into the session database
6. On Sundays: deep review (2-phase — fast pre-scan, then thorough analysis)

### Post-hooks

After cron completes:

- `summaries.json` → imported into SQLite database
- `report.txt` → sent via Telegram (if configured)
- Safety checks → warn on leaked secrets, soul file shrinkage
- Custom scripts in `hooks/cron.d/` are executed

## Heartbeat Mode

```bash
miro --heartbeat
```

### What it does

1. Checks health of configured services (HTTP endpoints)
2. Processes pending tasks from your task system (Jira, etc.)
3. Monitors behavioral patterns and anomalies
4. Reports status via Telegram

### Configuring health checks

Create `data/services.json`:

```json
[
  {
    "name": "API",
    "url": "http://localhost:8080/health",
    "timeout": 5
  },
  {
    "name": "Database",
    "url": "http://localhost:5432/",
    "timeout": 3
  }
]
```

## Evolve Mode

```bash
miro --evolve
```

### What it does

1. Reviews recent interactions with the user
2. Identifies areas for improvement in soul files
3. Makes small, targeted edits to `SOUL.md`, `USER.md`, etc.
4. Checks for bugs or tech debt in the workspace
5. Runs build and tests to verify changes
6. Reports what it changed via Telegram

!!! info "Self-modification with guardrails"
    The evolve mode can modify soul files, but:

    - `CORE.md` is protected — changes are auto-reverted by post-hooks
    - Soul file shrinkage >20% triggers a warning
    - Each change is small and incremental
    - Changes are recorded in daily notes for auditability

### Example evolve output

```
Evolve #4 Summary:
- Updated USER.md: added note about preference for table-formatted output
- Fixed typo in AGENTS.md memory section
- No code changes needed
- Disk usage: 85% (warning threshold not reached)
```

## Cron Setup

=== "crontab"

    ```crontab
    # Memory consolidation — every 4 hours
    0 */4 * * * PATH="$HOME/go/bin:$HOME/.local/bin:$PATH" miro --cron >> /tmp/miro-cron.log 2>&1

    # Heartbeat — every 2 hours
    30 */2 * * * PATH="$HOME/go/bin:$HOME/.local/bin:$PATH" miro --heartbeat >> /tmp/miro-heartbeat.log 2>&1

    # Self-evolution — daily at 10am
    0 10 * * * PATH="$HOME/go/bin:$HOME/.local/bin:$PATH" miro --evolve >> /tmp/miro-evolve.log 2>&1
    ```

=== "macOS launchd"

    ```xml title="~/Library/LaunchAgents/com.miro.cron.plist"
    <?xml version="1.0" encoding="UTF-8"?>
    <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
      "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
    <plist version="1.0">
    <dict>
      <key>Label</key>
      <string>com.miro.cron</string>
      <key>ProgramArguments</key>
      <array>
        <string>/usr/local/bin/miro</string>
        <string>--cron</string>
      </array>
      <key>StartInterval</key>
      <integer>14400</integer>
      <key>StandardOutPath</key>
      <string>/tmp/miro-cron.log</string>
      <key>StandardErrorPath</key>
      <string>/tmp/miro-cron.log</string>
    </dict>
    </plist>
    ```

!!! tip "PATH matters"
    Cron jobs run with a minimal PATH. Make sure both `claude` and your binary are findable — either use full paths or add them to the PATH prefix.

## Custom Hooks

Add shell scripts to `hooks/{cron,heartbeat,evolve}.d/` for custom post-processing:

```bash
mkdir -p ~/.openclaw/data/hooks/cron.d
```

```bash title="hooks/cron.d/notify-slack.sh"
#!/bin/bash
# Send cron summary to Slack
if [ -f "$SESSION_DIR/report.txt" ]; then
  curl -X POST "$SLACK_WEBHOOK" \
    -d "{\"text\": \"$(cat $SESSION_DIR/report.txt)\"}"
fi
```

```bash
chmod +x hooks/cron.d/notify-slack.sh
```

Hook scripts receive environment variables:

| Variable | Description |
|----------|-------------|
| `<APPNAME>_MODE` | `cron`, `heartbeat`, or `evolve` |
| `<APPNAME>_WORKSPACE` | Path to workspace |
| `<APPNAME>_DB` | Path to sessions.db |
| `SESSION_DIR` | Temp directory with session output |

## Telegram Notifications

Automated modes can send reports via Telegram. Configure:

1. Set `telegramChatID` in `config.json` or `<APPNAME>_TG_CHAT_ID` env var
2. Ensure the Telegram bot token is available (via OpenClaw credentials or config)

The report is generated by Claude during the automated session and saved as `report.txt`, then delivered by the post-hook system.
