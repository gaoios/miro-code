# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

`soul-cli` is a Go CLI that launches Claude Code with a "soul prompt". It assembles identity/memory/skill files into a system prompt, then `exec`s or subprocess-runs `claude` with that prompt. It also runs an HTTP server for persistent session management, handles cron-based memory consolidation, heartbeat health checks, Telegram integration, provider proxying, and a session summary database. The binary name is configurable — compile as `miro`, `soul`, or any name you want; all paths and env vars are derived from it.

## Build & Run

```bash
make              # build with ldflags version injection + codesign
make install      # build + install to ~/.local/bin/
make server-restart  # build + install + restart launchd service
go test ./...     # run all tests
go test -run TestBuildSkillIndex  # single test
```

**⚠️ Always use `make`, never bare `go build`** — ldflags inject version info and macOS codesign is required.

No external services needed for tests (uses temp dirs and in-memory SQLite). Some tests (e.g. `TestBuildSkillIndex`, `TestBuildPrompt`) read real files from `~/.openclaw/` so they only pass on machines with a configured workspace.

## Architecture

Multi-file Go program: application code in `internal/app` (single package `app`, ~29,000 lines moved 2026-09-18 from the former root `package main`), thin root `main.go` shim, plus `pkg/im` and `pkg/provider/*`.

### Source Files (35 non-test files)

| File | Lines | Responsibility |
|------|-------|----------------|
| **Server Core** | | |
| `server.go` | 2,101 | HTTP server, 35+ REST endpoints, auth middleware, config loading, graceful shutdown |
| `server_session.go` | 1,772 | Session lifecycle (create/destroy/rehydrate), TTL reaper, concurrent session limits, state machine |
| `server_proxy.go` | 1,903 | Provider proxy (OpenAI/GLM/MiniMax), OAuth token validation, telemetry capture, CC session ID tracking, S3 image upload |
| `server_telegram.go` | 1,331 | Telegram bot webhook, message relay, session↔chat association |
| `server_process.go` | 675 | Claude process spawning, stderr capture, exit handling |
| `server_rename.go` | 725 | Session auto-rename via AI, model selection |
| `server_ws.go` | 423 | WebSocket hub, per-client read/write pumps, broadcast |
| `server_stream.go` | 255 | SSE broadcaster for real-time session updates |
| `server_ipc.go` | 294 | Server-side IPC listener for peer session communication |
| `server_haiku_pool.go` | 213 | Lightweight Haiku model pool with idle reaper |
| `server_linkpreview.go` | 165 | Open Graph link preview fetching |
| **CLI Core** | | |
| `main.go` | 1,184 | Entry point, CLI arg parsing, command dispatch, help text |
| `claude.go` | 1,104 | Process locking (NFS-safe), `syscall.Exec`, `runClaude` subprocess, crash tracking |
| `init.go` | 625 | First-run setup wizard, archetype selection, workspace scaffolding |
| **Prompt & Skills** | | |
| `prompt.go` | 1,096 | Prompt assembly, token budgeting (100k limit), soul/memory/skill injection |
| `skills.go` | 346 | Skill/project index scanning, YAML frontmatter parsing |
| **Database & Search** | | |
| `db.go` | 928 | SQLite session tracking, hash-based change detection, pattern cultivation |
| `fts.go` | 866 | FTS5 full-text search, three-table indexing (daily/session/content) |
| `fts_seg.go` | 128 | Chinese text segmentation (gse jieba-style) |
| `sessions.go` | 595 | Session scanning, search, TUI entry point |
| **Task Dispatch & Evolution** | | |
| `spawn.go` | 992 | Agent spawning (async/sync), bare mode, agent discovery |
| `probe.go` | 970 | Evolve-probe: feedback rule testing, BM25 scenario judging |
| `tasks.go` | 478 | Cron/heartbeat/evolve task templates (text/template) |
| **Communication** | | |
| `ipc.go` | 467 | Local IPC client for delegating to running server |
| `telegram.go` | 167 | Telegram message/photo sending with token caching |
| `pkg/im/telegram.go` | 508 | Telegram bot API client (Update/Message/Chat types, webhook) |
| **Lifecycle & Config** | | |
| `hooks.go` | 638 | Post-hooks after cron/heartbeat/evolve, safety checks, TG reporting |
| `status.go` | 1,188 | Health checks, doctor diagnostics, config display, metrics anomaly detection |
| `session_lifecycle.go` | 261 | Session reset policies (idle/daily/both), TG notifications |
| `soul_session.go` | 293 | Persistent soul session compaction, interaction round limits |
| `versions.go` | 270 | Version history, safe build (backup→build→verify→rollback) |
| **Utilities** | | |
| `provider_openai.go` | 1,139 | Codex protocol proxy: Anthropic ↔ OpenAI tool protocol translation |
| `provider_ollama.go` | 793 | Anthropic→Ollama protocol proxy (Anthropic messages → OpenAI-compatible /v1/chat) |
| `tui.go` | 473 | Bubbletea TUI session picker (resume mode) |
| `resolve_secrets.go` | 97 | Vault reference resolution (vault://, env://) |
| `safe.go` | 118 | Symlink protection, NFS-safe file locking |
| `new.go` | 109 | Reset Telegram direct sessions |

### Test Files (23 files, ~6,200 lines)

| File | Lines |
|------|-------|
| `main_test.go` | 519 |
| `claude_test.go` | 518 |
| `db_test.go` | 498 |
| `sessions_test.go` | 372 |
| `safe_test.go` | 366 |
| `server_rename_test.go` | 339 |
| `prompt_test.go` | 326 |
| `fts_test.go` | 318 |
| `init_test.go` | 308 |
| `session_lifecycle_test.go` | 305 |
| `server_test.go` | 284 |
| `versions_test.go` | 273 |
| `tui_test.go` | 242 |
| `parseargs_test.go` | 234 |
| `hooks_test.go` | 216 |
| `fts_seg_test.go` | 176 |
| `status_test.go` | 166 |
| `resolve_secrets_test.go` | 156 |
| `skills_test.go` | 154 |
| `telegram_test.go` | 123 |
| `tasks_test.go` | 113 |
| `fts_probe_test.go` | 53 |
| `test_helpers_test.go` | 12 |

## Execution Modes

| Mode | Trigger | Process Model | Description |
|------|---------|---------------|-------------|
| `interactive` | default | `syscall.Exec` | Replace process with `claude`, soul as system prompt |
| `print` | `-p "task"` | `syscall.Exec` | One-shot task execution |
| `resume` | `-r [id]` | `syscall.Exec` | Resume session; bare `-r` opens TUI picker |
| `cron` | `--cron` | subprocess | Memory consolidation; Sunday two-phase deep review |
| `heartbeat` | `--heartbeat` | subprocess | Health check patrol, auto-destroy on completion |
| `evolve` | `--evolve` | subprocess | Self-evolution: code audit, soul update, build & commit |
| `server` | `server` | HTTP server | Long-running API server with persistent sessions |
| `db` | `db <sub>` | direct | Session database subcommands |
| `spawn` | `spawn` | HTTP/subprocess | Task dispatch to other agents |
| `evolve-probe` | `evolve-probe` | subprocess | Feedback rule scenario testing |
| `init` | `init` | interactive | First-run setup wizard |

## HTTP API Endpoints (Server Mode)

### Public (no auth)
- `GET /api/health` — Server status, version, uptime
- `POST /api/wake` — Wake from idle
- `POST /api/spawn` — External task dispatch
- `GET /` — Web UI (embedded HTML)
- `GET /uploads/<file>` — Static files

### Session Management (auth required)
- `GET /api/sessions` — List sessions
- `POST /api/sessions` — Create session
- `GET /api/sessions/{id}` — Session details
- `PATCH /api/sessions/{id}/rename` — Rename
- `POST /api/sessions/{id}/auto-rename` — AI auto-rename
- `DELETE /api/sessions/{id}` — Delete session
- `GET /api/sessions/{id}/interaction-count` — Turn count (with IPC)
- `GET /api/sessions/{id}/wait` — Long-poll for status change
- `POST /api/sessions/{id}/message` — Send message
- `POST /api/sessions/{id}/message-from` — IPC relay
- `POST /api/sessions/{id}/upload` — File upload
- `POST /api/sessions/{id}/voice` — Voice transcription (ffmpeg → whisper)
- `GET /api/sessions/{id}/stream` — SSE stream
- `POST /api/sessions/{id}/control` — Control (interrupt/pause/resume)
- `POST /api/sessions/{id}/chrome` — Chrome debugging
- `POST /api/sessions/{id}/replace-soul` — Replace soul files
- `POST /api/sessions/{id}/set-model` — Change model
- `POST /api/sessions/{id}/usage` — Track token usage
- `POST /api/sessions/resume` — Resume by ID

### History & Memory
- `GET /api/history` — Session histories
- `GET /api/history/{id}/messages` — Messages from JSONL

### Search
- `GET /api/search?q=&scope=&limit=` — FTS5 search

### Config & Models
- `GET /api/config` — Current config
- `GET /api/providers` — Provider models
- `GET /api/models` — All available models
- `GET /api/skills` — Skill index
- `GET /api/link-preview?url=` — OG preview

### Proxy & Usage
- `GET /api/proxy/openai?provider=` — Proxy status
- `GET /api/proxy/usage` — Token usage snapshot
- `GET /api/proxy/logs` — Request log
- `GET /api/proxy/stats` — Aggregated stats
- `GET /api/proxy/session-cost` — Per-session cost
- `GET /api/proxy/logs/filters` — Filter values
- `GET /api/proxy/logs/export` — CSV export
- `GET /api/glm/quota` — GLM quota
- `GET /api/minimax/quota` — MiniMax quota

### WebSocket
- `GET /api/ws` — Real-time session updates

### Admin
- `POST /api/server/prepare-restart` — Graceful restart

### GAL
- `GET /api/gal` — List GAL saves
- `GET /api/gal/{id}` — Get GAL save

## CLI Subcommands

### Core
- `miro` — Interactive session
- `miro -p "task"` — One-shot
- `miro -r [id]` — Resume (TUI picker if no ID)
- `miro --model <model>` — Override model
- `miro --standard` — Append mode

### Server
- `miro server [--port --host --token]` — Start HTTP API server

### Database (`miro db`)
- `db recall` / `db pending` / `db summarized` — Session scan status
- `db save '<json>'` / `db save-batch` — Save summaries
- `db list` / `db stats` — View records
- `db search <keyword>` — Search summaries
- `db gc` — Garbage collect
- `db patterns` / `db pattern-save` / `db pattern-save-batch` — Pattern management
- `db feedback '<json>'` — Record pattern feedback
- `db cultivate [--dry-run]` — Skill cultivation
- `db pattern-reject <name>` — Reject pattern
- `db fts-index` / `db fts-index-sessions` / `db fts-rebuild` — FTS indexing
- `db search-fts <query> [--scope --limit --json]` — FTS search

### Spawn
- `miro spawn <agent> "task" [--wait]` — Dispatch task
- `miro spawn --bare --model <model> --project <path> "task"` — Bare spawn
- `miro spawn list` / `log <id>` / `finish <id>` — Manage spawns

### Evolution
- `miro evolve-probe --feedback <name> --scenario <id>` — Probe feedback rule
- `miro evolve-probe --sample N` — Sample least-probed rules
- `miro evolve-probe --regression-archive` — Monthly regression

### Diagnostics
- `miro status` — Quick health
- `miro doctor [cron]` — Deep diagnostics
- `miro config` — Show config
- `miro log [N]` — View daily notes
- `miro diff` — Soul/memory changes
- `miro prompt` — Print assembled prompt with token stats
- `miro lint` — Validate markdown formats

### Build & Version
- `miro build` — Safe build (backup→build→verify→rollback)
- `miro versions` — List versions
- `miro rollback [N]` — Rollback
- `miro update` — Git pull + build

### Communication
- `miro notify [--dry-run] <message>` — Send Telegram text
- `miro notify-photo [--dry-run] <URL> [caption]` — Send Telegram photo
- `miro new` — Reset Telegram sessions
- `miro models` — List models

### Session Management (via server IPC)
- `miro session list` — List active sessions
- `miro session read <id>` — Read session history
- `miro session search <id> "keyword"` — Search session
- `miro session send <id> "message"` — Send message to session
- `miro session wait <id>` — Wait for session idle
- `miro session close <id>` — Destroy session

## Concurrency Model

### Background Goroutines (Server Mode)
1. **HTTP Server** — Main listener with graceful shutdown
2. **Proxy Server** — Separate listener for model proxy
3. **Session Reaper** — TTL cleanup for idle/expired sessions
4. **Haiku Pool Reaper** — Idle haiku process cleanup
5. **Rehydration** — Delay-loaded session restoration after startup
6. **Proxy Log Cleanup** — Auto-delete logs >30 days
7. **Telegram Relay** — Forward session updates to Telegram

### Per-Session Goroutines
8. **Exit Monitor** — Watch for Claude process exit
9. **Stderr Drainer** — Prevent pipe deadlock
10. **WebSocket Read/Write Pumps** — Per-client pair
11. **SSE Writer** — Per-subscriber event streaming

### Synchronization
- **Mutexes**: `sessionManager.mu`, `serverSession.mu`, `wsHub.mu`, `wsClient.mu`, `sseBroadcaster.mu`
- **Channels**: Broadcaster send queues (256-buf), waiter notify, signal channels
- **sync.Once**: S3 client init, Haiku pool singleton

## Key Paths & Constants

- `workspace` = `~/.openclaw/workspace` (soul files, memory, projects)
- `claudeBin` = `~/.local/bin/claude`
- `lockfile` = `/tmp/miro.lock` (prevents concurrent cron/heartbeat)
- `promptOut` = `/tmp/miro-prompt-active.md` (assembled prompt)
- `dbPath` = `<appHome>/data/sessions.db` (SQLite)
- `tgChatID` = Telegram chat ID (from config.json / openclaw.json / env)

## Key Dependencies

| Package | Version | Purpose |
|---------|---------|---------|
| `modernc.org/sqlite` | v1.48.1 | Pure Go SQLite (FTS5, triggers) |
| `github.com/gorilla/websocket` | v1.5.3 | WebSocket protocol |
| `github.com/charmbracelet/bubbletea` | v1.3.10 | Terminal TUI framework |
| `github.com/aws/aws-sdk-go-v2` | v1.41.5+ | S3 client (Wasabi image upload) |
| `github.com/go-ego/gse` | v1.0.2 | Chinese text segmentation |
| `github.com/google/uuid` | v1.6.0 | Session ID generation |
| `gopkg.in/yaml.v3` | v3.0.1 | YAML parsing |

## Coding Rules

- **Framework vs user data separation**: Framework code must only match text produced by the framework itself (e.g. templates in `tasks.go`). User skill names, prompt content, and behavioral patterns are private data — never hardcode them into framework logic. soul-cli is open-source; code must not leak any specific user's usage patterns.
- **Build discipline**: Always use `make` for builds. Never bare `go build` — ldflags and codesign are required.
- **Server restart**: Use `make server-restart` which handles build + install + launchctl stop/start.

## Hooks

Shell scripts in `hooks/{cron,heartbeat}.d/` run after each cron/heartbeat session. They receive env vars: `MIRO_MODE`, `MIRO_WORKSPACE`, `MIRO_DB`. Currently `90-safety-extended.sh` does supplementary checks (memory pollution, git health, temp file cleanup, openclaw.json validity).

## Review Priority (by risk)

High-risk modules that have had recent bugs:
1. `server_session.go` — nil channel panic, resume model loss, rehydrate race conditions
2. `server_proxy.go` — protocol translation edge cases, token tracking accuracy
3. `claude.go` + `server_process.go` — process lifecycle, crash tracking
4. `server.go` — endpoint auth, config loading
5. `provider_openai.go` — Codex protocol translation, JWT refresh
