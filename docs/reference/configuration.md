# Configuration Reference

## config.json

Located at `<workspace>/scripts/<appName>/config.json` or `<appHome>/data/config.json`.

```json
{
  "jiraToken": "",
  "telegramChatID": "",
  "agentName": "",
  "projectRoots": [
    "~/projects",
    "~/work"
  ],
  "agents": {
    "codex": {
      "enabled": true,
      "binary": "codex",
      "model_map": {"implementation": "gpt-5.5"},
      "dispatch_hint": "Use for implementation-heavy engineering work."
    }
  },
  "tools": {
    "meoo": {
      "enabled": true,
      "binary": "meoo",
      "capabilities": ["build applications", "deploy static sites"],
      "dispatch_hint": "Verify authentication before deployment."
    }
  },
  "server": {
    "token": "",
    "host": "127.0.0.1",
    "port": 9847,
    "maxSessions": 5,
    "idleTimeoutMin": 30,
    "maxLifetimeHours": 4
  }
}
```

### Top-level fields

| Field | Type | Description | Env Override |
|-------|------|-------------|--------------|
| `jiraToken` | string | Token for Jira-like task system | `JIRA_TOKEN` |
| `telegramChatID` | string | Telegram chat ID for notifications | `<APPNAME>_TG_CHAT_ID` |
| `agentName` | string | Display name (defaults to binary name) | `AGENT_NAME` |
| `projectRoots` | string[] | Directories to scan for `CLAUDE.md` files | — |
| `agents` | object | Optional AI backend configuration | — |
| `tools` | object | Non-conversational CLIs available to workers | — |

### Agent backend fields

Supported keys under `agents` are `codex`, `grok`, `kimi`, and `agy`.

| Field | Type | Description |
|-------|------|-------------|
| `enabled` | bool | Enables explicit selection and model auto-routing |
| `binary` | string | Executable name or path |
| `model_map` | object | Local alias to backend-native model name |
| `dispatch_hint` | string | Local quota, role, cost, or priority guidance for the supervisor |

Codex additionally supports `permission_profile` and `approval_policy`.
`permission_profile` accepts `"workspaceWrite"` (default, maps to
`sandbox: "workspace-write"`), `"readOnly"`, or `"dangerFullAccess"`; see
[codex-backend-runbook.md §8.5](../codex-backend-runbook.md) for the wire
mapping and the codex 0.153.x protocol change.
Antigravity additionally supports `mode` (`plan` or `accept-edits`), `effort`,
and `dangerously_skip_permissions`. The dangerous permission flag defaults to
`false` and should only be enabled in trusted workspaces.

### Tool CLI fields

Entries under `tools` are ordinary executables, not AI backends.

| Field | Type | Description |
|-------|------|-------------|
| `enabled` | bool | Advertises the tool to the supervisor prompt |
| `binary` | string | Executable name or path |
| `capabilities` | string[] | Short descriptions used for task routing |
| `dispatch_hint` | string | Preconditions and usage guidance |

### Server fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `server.token` | string | — | **Required** for server mode. Auth token | 
| `server.host` | string | `127.0.0.1` | Bind address |
| `server.port` | int | `9847` | Listen port |
| `server.maxSessions` | int | `5` | Max concurrent Claude Code sessions |
| `server.idleTimeoutMin` | int | `30` | Reap idle sessions after N minutes |
| `server.maxLifetimeHours` | int | `4` | Hard session lifetime limit |

## Environment Variables

All env vars use `<APPNAME>` as prefix, where `APPNAME` is the binary name in UPPER_CASE (e.g., binary `myai` → prefix `MYAI`).

| Variable | Description | Default |
|----------|-------------|---------|
| `<APPNAME>_HOME` | Base directory for all data | `~/.openclaw` |
| `<APPNAME>_TG_CHAT_ID` | Telegram chat ID | — |
| `<APPNAME>_SERVER_TOKEN` | Server auth token | — |
| `JIRA_TOKEN` | Jira API token | — |
| `AGENT_NAME` | Override agent name | Binary filename |

## Name Resolution

The agent name is resolved in priority order:

| Priority | Source | Example |
|----------|--------|---------|
| 1 | Build-time ldflags | `-X main.defaultAppName=myai` |
| 2 | `AGENT_NAME` env var | `AGENT_NAME=myai ./soul-cli` |
| 3 | Binary filename | `./myai` (from `os.Args[0]`) |

## Directory Structure

| Path | Description |
|------|-------------|
| `<appHome>/` | Base directory (`~/.openclaw` or `<APPNAME>_HOME`) |
| `<appHome>/workspace/` | Soul files, memory, projects |
| `<appHome>/data/` | App data (sessions.db, metrics, hooks, versions) |
| `<appHome>/data/config.json` | Configuration file (alternative location) |
| `<appHome>/data/sessions.db` | SQLite session database |
| `<appHome>/data/metrics.jsonl` | Run metrics log |
| `<appHome>/data/services.json` | Health check targets |
| `<appHome>/data/hooks/` | Post-run hook scripts |
| `<appHome>/data/.versions/` | Binary version history |

## OpenClaw Integration

When `openclaw.json` exists in the home directory, soul-cli reads:

- **Workspace path** from the matched agent config
- **Agent name** from `agents.list[].name`
- **Telegram bot token** from OpenClaw credentials
- **Telegram conversation context** from the active session

This is optional. Without OpenClaw, everything is configured via `config.json` and environment variables.
