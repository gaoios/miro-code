# Multiple Agents

soul-cli supports running multiple independent AI agents from a single codebase. Each agent gets its own identity, workspace, database, and lock file.

## How It Works

The binary name determines the agent's identity. Build a separate binary for each agent:

```bash
go build -ldflags "-X github.com/kiyor/soul-cli/internal/app.defaultAppName=atlas"    -o atlas .     # main assistant
go build -ldflags "-X github.com/kiyor/soul-cli/internal/app.defaultAppName=sentinel"  -o sentinel .  # health monitor
go build -ldflags "-X github.com/kiyor/soul-cli/internal/app.defaultAppName=worker"    -o worker .    # task executor
```

Each binary automatically gets isolated:

| Resource | `atlas` | `sentinel` |
|----------|---------|------------|
| Home dir | `~/.atlas/` | `~/.sentinel/` |
| Data dir | `~/.atlas/data/` | `~/.sentinel/data/` |
| Database | `~/.atlas/data/sessions.db` | `~/.sentinel/data/sessions.db` |
| Lock file | `/tmp/atlas.lock` | `/tmp/sentinel.lock` |
| Env prefix | `ATLAS_HOME` | `SENTINEL_HOME` |
| Log prefix | `[atlas]` | `[sentinel]` |

## Shared Workspace, Separate Data

Multiple agents can **share a workspace** (soul files, memory) while maintaining separate operational data:

```
~/.openclaw/
├── workspace/              ← shared soul files, memory
│   ├── SOUL.md
│   ├── USER.md
│   └── memory/
├── data/                   ← atlas's data (default agent)
│   ├── sessions.db
│   └── hooks/
└── agents/
    ├── sentinel/data/      ← sentinel's data
    └── worker/data/        ← worker's data
```

Or give each agent its own workspace entirely:

```bash
export ATLAS_HOME=~/agents/atlas
export SENTINEL_HOME=~/agents/sentinel
```

## With OpenClaw

If you're running [OpenClaw](https://github.com/nicepkg/openclaw), each binary automatically matches its agent config from `openclaw.json` by ID:

```json title="openclaw.json (simplified)"
{
  "agents": {
    "list": [
      { "id": "atlas", "name": "Atlas", "workspace": "..." },
      { "id": "sentinel", "name": "Sentinel", "workspace": "..." }
    ]
  }
}
```

The binary named `sentinel` finds `agents.list[].id == "sentinel"` and reads its workspace path and configuration.

## Example: 3-Agent Setup

```bash
# Main assistant — interactive use
atlas                        # interactive session
atlas -p "review PR #42"     # one-shot task

# Health monitor — automated
sentinel --heartbeat         # check service health
sentinel --cron              # consolidate sentinel's memory

# Task worker — one-shot tasks from a queue
worker -p "deploy staging"
worker -p "run migration"
```

### Cron for all agents

```crontab
# Atlas: memory consolidation every 4 hours
0 */4 * * * PATH="$HOME/go/bin:$PATH" atlas --cron

# Sentinel: heartbeat every hour
0 * * * * PATH="$HOME/go/bin:$PATH" sentinel --heartbeat

# Atlas: daily self-evolution
0 10 * * * PATH="$HOME/go/bin:$PATH" atlas --evolve
```

## Different Personalities

Each agent can have different soul files. Give them different workspaces:

```bash
# Atlas: thoughtful, thorough
cat > ~/agents/atlas/workspace/SOUL.md << 'EOF'
## Personality
- Thorough and methodical
- Explains reasoning before acting
- Prefers safety over speed
EOF

# Worker: fast, action-oriented
cat > ~/agents/worker/workspace/SOUL.md << 'EOF'
## Personality
- Fast and direct
- Do first, explain if asked
- Minimal output, maximum action
EOF
```
