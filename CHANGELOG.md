# Changelog

## Unreleased

### Multi-CLI Agent Workspace

- Added first-class server backends for Codex, Grok Build, Kimi CLI, and
  Antigravity CLI alongside the existing Claude Code backend.
- Added explicit backend selection, model-prefix and `model_map` routing,
  unified events, Web UI backend visibility, session persistence, and smoke
  tests for alternate workers.
- Added Claude supervisor dispatch policy with per-worker `dispatch_hint`
  overrides. Local quota and role preferences stay in private configuration
  instead of being hard-coded in source.
- Added a generic `tools` registry for non-conversational CLIs shared by
  workers. Deployment and media-generation tools such as Meoo or a compatible
  Jimeng CLI can be described without pretending to be AI model backends.

## v1.12.0

### Tmux Drawer (Web UI)

- **Read-only tmux session snoop** in the right-side drawer: lists active panes with last activity, opens individual pane scrollback. Alpine.js declarative port (-115 LOC net vs imperative).
- **Subagent panel backfill**: `/api/sessions/{id}/subagents` endpoint + frontend backfill on session switch — Claude Code native subagents now show in the drawer with full history (prior version showed `1/1` with no list).

### DB Explorer UI

- **`/api/db/{sources,tables,schema,query,ui}`**: read-only SQLite explorer. SELECT-only with row cap + timeout. Minimal embedded HTML at `/api/db/ui` (textarea + Run + table) for ad-hoc queries against `sessions.db` and friends.

### Tool-Hook System

- **`tool_hook` subcommand**: PreToolUse + PostToolUse path-aware system-reminder injection. YAML rules in `tool-hooks.yaml` matched against tool name + path globs, audit table in `sessions.db`.
- **Event dispatcher**: covers PreToolUse, PostToolUse, UserPromptSubmit, Stop, SessionStart, SessionEnd. All events audited.
- **`tool-hook stats / search`**: per-rule hit counts and full-text search across audit log; weekly cleanup of zero-hit rules.
- **`tool-hook disable`**: temporarily silence rules without editing YAML.
- **Skip-input mirror**: PostToolUse handler now respects `skip_input` the same way PreToolUse does (was missing — fixed after a degraded session showed 5 ignored corrections).

### Evolve Cycle Log

- **`evolve_db`**: persists structured evolve cycle summaries (mode, started_at, files_touched, errors, charter_drift) to SQLite — queryable history of self-evolution runs without grepping markdown reports.
- **Charter enforcement**: evolve refuses to roll back uncommitted changes; `srcDir` auto-detection; per-skill telemetry (which skills fired, hit rates).

### Provider Refactor

- **`pkg/provider/{openai,gemini,ollama}`**: embedded provider proxies extracted into separate packages — clearer ownership, easier to test in isolation.
- **Gemini provider**: server-hosted gemini proxy wired through `resolveProvider` + `injectProviderEnv`. `--model gemini/...` routes correctly.
- **Ollama provider**: Anthropic→Ollama protocol proxy (Anthropic messages → OpenAI-compatible `/v1/chat`).

### Proxy / Translator Hardening

- **Always route through local monitoring proxy**, regardless of OAuth state — telemetry is never lost.
- **OAuth warming**: keep OAuth token validated in background; only inject proxy URL when token is valid.
- **Translator gap fixes**: `stop_reason` mapping, `thinking` blocks, `effort` → `thinking` mapping, tool stripping for non-tool-capable models.
- **Codex error mapping**: proper error type + `Retry-After` header propagation.
- **gpt-5.4 family**: downgrade `effort=minimal` to `none` (model rejects `minimal`).
- **TZ fix**: usage 1h/6h window filter was silently broken by mac/linux TZ mismatch.
- **Concurrent-map panic fix**: proxy log writes serialized.

### Session & UI

- **Stable session ID across resume & rehydrate**: weiran session ID persists, decoupled from underlying CC session ID.
- **Session mode enum**: `weiran` / `benwo` / `cc` — explicit mode field instead of inferring from flags. CC sessions get stable URL by `cc-id`.
- **Compact UI**: subagent drawer, path completion in input box, batch session meta fetch (1 round trip vs N), dynamic branding from config.
- **Session search optimizations**: recent-cmd shortcut, timestamp fix, auto-index on first search.
- **`netprobe` subcommand**: built-in network reachability probe (ICMP + TCP + DNS) for diagnosing proxy/upstream issues.
- **Web split**: CSS + components moved to separate files; Alpine.js added for declarative panels.
- **AUQ auto-skip**: AskUserQuestion auto-skip when input is unambiguous.
- **Ctx % peak restore**: context window peak indicator restored across resumes.

### Reliability

- **JSONL scanner buffer bump**: 64 MB max line size — large tool result blocks no longer truncate.
- **Background task tracker**: `/api/sessions/{id}/state` endpoint exposes tool calls in flight, queue depth, last activity.
- **`limitedBuffer` data race fix**, **flaky TG test fix**.
- **Linux bootstrap** + deployment guide; `claudeBin` PATH fallback for non-mac hosts.
- **Model fallback chain**: when primary model fails, fall back through configured chain; opus[1m] context window suffix preserved.

### Security & Code Review

- Multiple rounds of code review fixes: template injection, model validation, bridge cleanup, concurrency bugs, nil panics, FTS segmentation edge cases.

### Open-Source Adaptation

- **Stripped personal hardcodes**: agent nick (`weiran` / `未然`), owner name, vendor string now come from compile-time `-ldflags` knobs (`agentNick`, `ownerName`, `VENDOR`) — same binary buildable as `weiran`, `soul`, or any other identity.

### Session Rehydration (Server Restart Resilience)

- **Graceful shutdown persistence**: `shutdownAll()` now saves `claude_session_id` and model for all active sessions before killing processes, marks them as `suspended` in DB
- **Auto-rehydrate on startup**: `rehydrateSessions()` runs 3s after server start — finds `suspended`/`active` sessions (within 2h), resumes them via `--resume` with context-aware messages
- **Restart initiator flow**: `POST /api/server/prepare-restart` + `{cli} session prepare-restart` lets a session mark itself as the restart trigger, receives a custom wake message after rehydration
- **Bystander sessions**: Non-initiator sessions receive a server-restart warning ("in-flight tool calls were killed, do NOT assume they succeeded")
- **DB migrations**: `status`, `rehydrate_message`, `model` columns added to `server_sessions`
- **`destroySessionForShutdown()`**: Separate shutdown path that preserves `suspended` DB status (vs `destroySession` which marks `ended`)
- **Stale session expiry**: `expireStaleRehydratables()` cleans up sessions older than `rehydrateMaxAge` (2h)

### Image Upload + S3

- **Image content blocks**: `sendMessage()` now detects `![alt](url)` in messages, reads images (local file or HTTP URL), converts to base64, and sends as Claude SDK image content blocks — LLM can actually "see" uploaded images
- **S3 upload**: Upload handler uploads to Wasabi S3 (best-effort, fallback to local `/uploads/`). Config via `server.s3` in config.json
- **S3 config**: `endpoint`, `bucket`, `region`, `prefix`, `profile` fields using AWS SDK v2

### Session Wait

- **`{cli} session wait <id>`**: Block until target session reaches idle/exited state (10min default timeout, customizable via `?timeout=` query param)
- **Server-side waiters**: `GET /api/sessions/{id}/wait` uses channel-based notification — no polling, instant response on state change
- **Session waiter notifications**: `setStatus()` fires waiter channels when session reaches `idle`/`stopped`/`error`

### Spawn Delegation

- **Server-aware spawn**: `weiran spawn` detects running server and delegates via `POST /api/sessions` instead of direct `exec` — sessions appear in WebUI with proper lifecycle tracking

### Avatar & Welcome Image

- **Config fields**: `avatarUrl`, `userAvatarUrl`, `welcomeImage` in config.json — served via `/api/health` for WebUI
- **User avatar in chat**: User message bubbles display image avatar instead of letter initial
- **Full-body welcome image**: Welcome page shows agent's full-body illustration when no session is selected (responsive, portrait-optimized)

### Theme System (Web UI)

- **6 themes**: Midnight (default dark), Light, Sakura (粉色), Terminal (modern IDE dark), ACNH (Animal Crossing 粉蓝), Morandi (粉绿莫兰迪)
- **CSS variable architecture**: All colors use theme variables — `--badge-bg`, `--cost-color`, `--active-ov`, `--toggle-off`, `--hljs-theme`, etc.
- **Dynamic highlight.js theme**: Code blocks switch between `github-dark` and `github` based on `--hljs-theme` variable
- **Todo drawer**: Right-side slide-out panel for todo list (progress bar, themed styling)
- **Touch-safe interactions**: Swipe gesture exclusion zones for hamburger button and interactive elements on mobile

### Model Handling

- **`mergeInitModel()`**: Preserves user-specified context window suffix (e.g. `[1m]`) when Claude Code init message reports the base model without it
- **Model persistence**: `setSessionModel()` saves model to DB for rehydration, called on create/resume/setModel
- **`resumeSession` model parameter**: Resume now accepts and applies model override

### IPC Improvements

- **IPC env injection in resume**: Resumed sessions now get `{CLI}_SESSION_ID`, `{CLI}_SERVER_URL`, `{CLI}_AUTH_TOKEN` env vars (previously only new sessions got them)
- **Deduplicated IPC prefix**: Frontend extracts session name from `[From session xxx (name)]` prefix, displays in header, removes from body
- **JSON detection**: IPC messages that are valid JSON rendered as formatted code blocks

### Fixes

- **Resume session model override**: `resumeSession` accepts optional `model` parameter for provider-specific routing on rehydration
- **`message` alias in create API**: `POST /api/sessions` now accepts `message` as alias for `initial_message`
- **Hardcoded dark colors → theme vars**: Model badges, cost badges, CWD badge, todo items, avatars all use CSS variables instead of hardcoded `rgba()`

## v1.11.0

### Session IPC (Inter-Process Communication)

- **`{cli} session` subcommand family**: Full IPC between concurrent server sessions
  - `session list` — list active sessions (ID, name, status, model, marks own session)
  - `session read <id>` — read a session's full message history
  - `session search <id> "keyword"` — FTS search within a session's history
  - `session send <id> "message"` — inject a user message into another session's stdin (wakes idle sessions)
  - `session close <id>` — destroy a session (self-close protection)
- **Anti-loop enforcement**: Per-pair bidirectional interaction counter (default 10 rounds), HTTP 429 on exceed
- **Participant tracking**: `participants` field on session records which sessions have sent IPC messages
- **Short ID resolution**: All IPC commands accept UUID prefix (e.g. `b265` → full UUID)
- **WebSocket broadcast**: IPC events (`ipc_message`) broadcast to connected UI clients
- **Dynamic env var prefix**: IPC env vars (`{CLI}_SERVER_URL`, `{CLI}_AUTH_TOKEN`, `{CLI}_SESSION_ID`) derived from binary name at runtime — `weiran` → `WEIRAN_*`, `my-soul` → `MY_SOUL_*`

### OpenAI / GPT Model Support

- **GPT provider integration**: `--model gpt/gpt-5.4` routes through Claude Code's OAuth proxy to OpenAI models
- **Provider auto-detection**: Recognizes `gpt/` prefix alongside existing `minimax/`, `zai/` providers

### FTS5 Full-Text Search

- **SQLite FTS5 integration**: New `daily_notes` table + `daily_notes_fts` virtual table for keyword search across all daily notes (memory/*.md). External-content mode — no data duplication, triggers keep FTS in sync.
- **Session summaries FTS**: `session_summaries_fts` virtual table over existing `sessions.summary` column, also via external content + triggers.
- **Session content FTS**: `session_content_fts` indexes extracted user/assistant text from JSONL session files (both OpenClaw and Claude Code formats).
- **`weiran db fts-index`**: Scan and index all daily notes (incremental: skips unchanged files via mtime+hash).
- **`weiran db fts-index-sessions`**: Index session JSONL content only.
- **`weiran db search-fts <query>`**: BM25-ranked keyword search with `[highlighted]` snippets. Scope: `daily`, `session`, `content`, or `both`.
- **`weiran db fts-rebuild`**: Rebuild FTS5 indexes from scratch (escape hatch for corruption).
- **`GET /api/search`**: HTTP endpoint for FTS5 search (auth required). Query params: `q`, `scope`, `limit`.
- **Cron hook**: `indexDailyNotes()` runs after every cron memory consolidation, keeping the index fresh.
- **Query sanitization**: User queries with dots, hashes, CJK characters are auto-quoted for safe FTS5 MATCH.

### Session Lifecycle Automation (Jira #844)

- **`SessionResetPolicy`**: Configurable idle expiry + daily reset. Modes: `idle`, `daily`, `both`, `none`.
- **Background watcher goroutine**: `sessionLifecycleWatcher` runs inside `weiran server`, polls every 5min. Singleton-guarded, cancels cleanly on SIGTERM.
- **Idle expiry**: Parameterized `expireIdleSoulSessions(idleMinutes)` replaces hardcoded 24h. Default: 1440min (24h).
- **Daily reset**: `maybeDailyReset(atHour)` ends all active soul sessions once per day at configurable hour (default: 04:00 local). Idempotent via `lifecycle_kv` table.
- **Config**: `server.sessionReset` block in `config.json` — `mode`, `idleMinutes`, `dailyAtHour`, `notifyOnReset`.
- **Telegram notification**: Optional notification on reset via existing `sendTelegram()`.
- **Backward compat**: Existing `endStaleSoulSessions()` in soul_session.go untouched — heartbeat/cron callers still work.

### Framework Template System

- **`{CLI}` template variable**: Uppercase app name for env var references in FRAMEWORK.md (e.g. `{CLI}_SERVER_URL` → `WEIRAN_SERVER_URL`)
- **Existing `{cli}`**: Lowercase binary name (e.g. `{cli} session list` → `weiran session list`)

### Fixes

- **Provider mode leaks `CLAUDE_CODE_OAUTH_TOKEN` into third-party endpoints** (`server_proxy.go::injectProxyEnvWithModel`): When `--model provider/model` was active, the function still injected `CLAUDE_CODE_OAUTH_TOKEN` at the end, so Claude Code's interactive login check would prefer that token over the provider's API key and ship it to non-Anthropic endpoints (MiniMax, etc.), producing 401 / "login required" errors. Fix: strip `CLAUDE_CODE_OAUTH_TOKEN` entirely whenever a provider override is applied.
- **Stop injecting `CLAUDE_CONFIG_DIR` default** in interactive mode — let Claude Code use its own default.

### Model Discovery & Validation

- **`weiran models` subcommand**: Lists every available model grouped by provider — native Anthropic aliases (opus/sonnet/haiku) plus custom `provider/model` combos from `config.json`. Shows endpoint, auth env, and the default model used by cron/heartbeat/evolve.
- **Model-name validation warning**: `--model provider/model` now warns loudly to stderr when the model name is not in the provider's `models` whitelist.
- **`loadAllProviders` helper**: Shared reader for the providers section of `config.json`, reused by `resolveProvider` and `handleModels`.

## v1.10.0

### Id Mode (本我模式)

- **Default `--system-prompt-file`**: Soul prompt now *replaces* Claude Code's native system prompt instead of appending to it — strips CC's intro/tone/guidance, leaving only the soul identity
- **`--standard` flag**: Reverts to `--append-system-prompt-file` (old behavior) for compatibility
- **`--id` / `--soul` flags**: Explicit no-op aliases for default Id Mode
- **Server-side `ReplaceSoul` option**: `sessionCreateOpts.ReplaceSoul` threads through to `spawnClaude` → `sessionOpts.ReplaceSoul`

### Multi-Provider Model Routing

- **`--model provider/model` CLI flag**: e.g. `weiran --model zai/glm-5.1`, `weiran --model minimax/MiniMax-M2.7`
- **`providers` config section** in `config.json`: `baseUrl`, `apiKey`, `authEnv`, `models` per provider
- **`resolveProvider` / `injectProviderEnv`**: Looks up provider config, injects `ANTHROPIC_BASE_URL` + auth env, bypasses local proxy
- **`defaultModel` in config.json**: Applies to cron/heartbeat/evolve modes automatically; override via `WEIRAN_DEFAULT_MODEL` env var
- **`GET /api/providers`**: Server endpoint lists providers (apiKey redacted) + `defaultModel` for UI model dropdown
- **`providerModelName`**: Strips `provider/` prefix when passing `--model` to Claude Code

### Soul Session Persistence

- **`soul_sessions` DB table**: Tracks per-agent daily soul sessions (claude session ID, last-touch, budget)
- **Heartbeat resume**: On each heartbeat run, checks for an active soul session and passes `--resume <id>` to Claude — continuity across 24h window
- **`endStaleSoulSessions`**: Expires sessions inactive >24h
- **Server wake integration**: `/api/wake` (heartbeat trigger) participates in soul session lifecycle — resumes or creates, async-links Claude session ID
- **`detectNewSession`**: Polls JSONL files after run to find the newly-created Claude session ID for linking

### Voice Message Transcription

- **Telegram voice/audio handling**: Downloads OGG via Telegram file API, converts with `ffmpeg → whisper-cli`
- **Fast path**: `ffmpeg -ar 16000 -ac 1 → whisper-cli --language auto --no-timestamps` with timeout (30s + 2×duration)
- **Echo transcript**: Sends `📝 "transcript"` back to user for verification before passing to Claude
- **Delegation fallback**: If tools missing, builds a detailed self-install prompt for Claude to handle the audio itself
- **Model**: Prefers `ggml-small.bin`, falls back to `ggml-base.bin`

### Telegram Improvements

- **Streaming plain text**: `sendOrEditPlain` uses no `parse_mode` during streaming to avoid Telegram rejecting incomplete markdown; final `flush()` uses Markdown mode
- **Shutdown drain**: `drainQueue` processes remaining messages before consumer goroutines exit; `queueWg` coordinates clean shutdown
- **`SendMessagePlain` / `EditMessagePlain`** in `pkg/im/telegram.go`: Plain-text variants for streaming use

### OAuth Token Sharing

- **`CLAUDE_CODE_OAUTH_TOKEN` injection**: All spawned Claude processes share one static OAuth token — prevents race conditions from concurrent OAuth refreshes
- **Priority**: env var → `workspace/.oauth-token` file → `config.json server.oauthToken`

### `doctor cron` Subcommand

- **`weiran doctor cron`**: Audits crontab entries — binary path staleness, schedule sanity (heartbeat/cron/evolve coverage), log file health, evolve-probe readiness, recent run metrics

### `evolve-probe` Subcommand

- **`probe.go`**: Thought-experiment probes against feedback rules (v2 frontmatter `test_scenarios`)
- **`weiran evolve-probe -f <feedback> -s <scenario> [--mode with|without|both]`**: Runs a probe against one scenario
- **`--list`**: Lists scenarios for a feedback
- **`--sample N`**: Probes N least-recently-probed active feedbacks
- **`--regression-archive`**: Monthly probe of all archived rules
- **Judge**: haiku model auto-judges PASS/FAIL; updates `probe_pass_streak` in frontmatter
- **Proposals**: Archive/regression proposals written to `memory/evolve/proposals-YYYY-MM-DD.md`

### Evolve Template Overhaul

- **Phase 0**: Review recent interactions (unchanged)
- **Phase 1**: Invariant Check — scans `invariants.yaml`, hard safety check, sends notify on violation
- **Phase 1.5**: Fact Drift Reconciler — detects stale values across workspace files from git diff
- **Phase 2**: New Feedback Detection — scans daily notes for correction keywords, creates drafts in `memory/evolve/new/` (human approval required)
- **Phase 3**: Active Feedback Probing — `weiran evolve-probe --sample 3`
- **Phase N**: Soul & Memory Evolution (renumbered)
- **Wrap Up**: Report template now includes invariant/fact-drift/feedback/probe summary fields

### Miscellaneous

- **`doctor` passes extra args**: `parseArgs` now forwards extra args after `doctor` subcommand
- **`gopkg.in/yaml.v3`** added as dependency (for probe.go frontmatter parsing)

## v1.9.0

### Spawn System

- **`weiran spawn <agent> "task"`**: Dispatch tasks to other agents asynchronously, with `--wait` for synchronous mode
- **`weiran spawn list`**: Show running/recent spawn processes
- **Per-agent mutex**: Prevents concurrent spawns for the same agent (checks PID liveness, auto-marks stale as failed)
- **DB persistence**: `spawns` table tracks agent, task, PID, exit code, duration, output tail
- **Model passthrough**: Spawned agents use model from config.json (not hardcoded Opus)
- **TG notifications**: Completion/failure notifications with duration sent via Telegram

### Telegram Bridge

- **Native Telegram bot integration**: `pkg/im/telegram.go` — pure Go Telegram Bot API client with long-polling
- **`server_telegram.go`**: Bridges Telegram DM to Claude Code sessions — persistent per-chat sessions with auto-resume
- **Per-session environment override**: Telegram sessions get Telegram-specific prompt (concise messaging, photo handling, component conversion)
- **Photo support**: User photos downloaded locally, path injected as `[User sent a photo: /tmp/tg-photo-xxx.jpg]`
- **Message edit debounce**: 800ms debounce to avoid Telegram rate limits on streaming edits
- **Summary generation**: Auto-generates conversation summaries every 20 turns, stored in `memory/telegram/`
- **Allowed chat ID filtering**: Config-based whitelist for Telegram chat access

### Sniff Proxy (API Traffic Inspector)

- **`server_proxy.go`**: Transparent reverse proxy for Anthropic API — captures rate-limit headers, token usage, request/response metadata
- **Usage dashboard**: Real-time input/output token tracking with timeline visualization and "now" indicator
- **Request log**: Paginated, filterable request history (model, status, tokens, duration)
- **`ANTHROPIC_BASE_URL` injection**: `injectProxyEnv` automatically routes Claude Code through the sniff proxy
- **Config**: `server.proxy.enabled/port/upstream` in config.json

### Heartbeat Delegation

- **`delegateToServer`**: Heartbeat cron detects running server and delegates via `POST /api/wake` instead of spawning a subprocess — fixes session category misclassification
- **Conflict handling**: Returns 409 if heartbeat session already running, cron gracefully skips

### File Upload

- **Multipart upload API**: `POST /api/sessions/{id}/upload` — 32MB max, saves to `workspace/uploads/`
- **Web UI**: Drag-drop, paste, and button upload with thumbnail preview
- **Static file serving**: `/uploads/` route serves uploaded files
- **Unique filenames**: Crypto-random hex prefix prevents collisions

### Prompt System

- **Conditional HEARTBEAT.md**: Only injected in heartbeat/cron modes, not interactive/server sessions (reduces noise)
- **`injectServerModeContext2`**: Per-session environment override for Telegram vs Web UI context

### Database & Concurrency

- **SQLite WAL mode**: `PRAGMA journal_mode=WAL` for better concurrent read/write
- **Busy timeout**: `PRAGMA busy_timeout=5000` — wait 5s for lock instead of failing
- **`session_agents` table**: Maps Claude session IDs to agent identities
- **`spawns` table**: Persistent spawn process tracking

### Notify

- **`--dry-run` flag**: `weiran notify --dry-run` and `weiran notify-photo --dry-run` preview without sending

### Web UI

- **Sniff panel**: Usage stats + request log as a new tab alongside sessions
- **Upload button**: Paperclip icon in chat input, with thumbnail preview strip
- **CWD badge**: Shows session working directory
- **Hash routing**: `#/usage`, `#/logs` deep links
- **Fix**: Chat input no longer disabled after closing usage/log panel

## v1.8.0

### Session Categories & Lifecycle

- **Session categories**: `interactive`, `heartbeat`, `cron`, `evolve` — ephemeral categories (heartbeat/cron/evolve) auto-destroy on completion, don't count toward maxSessions
- **Session tags**: Freeform labels for filtering, stored in DB as JSON array
- **Category filter API**: `GET /api/sessions?category=interactive` filters session list
- **DB migrations**: Added `chrome_enabled`, `gal_id`, `category`, `claude_session_id`, `tags` columns (idempotent ALTER TABLE)

### GAL (Visual Novel) System

- **GAL session support**: `gal_id` field on session create/snapshot, links sessions to GAL save files
- **GAL replay mode**: `skip_replay` WS flag lets frontend handle history display for muted replay styling
- **GAL context injection**: `galContext` global for injecting save JSON into prompt

### Chrome Remote Debugging

- **`--chrome` flag**: Pass-through to Claude Code for Playwright browser control
- **Runtime chrome toggle**: `POST /api/sessions/{id}/chrome` reloads process with `--chrome` (suppressClose prevents UI disruption)
- **Chrome flag persistence**: `ChromeEnabled` field on session struct, passed through on resume/reload

### Prompt System

- **`weiran prompt`**: New subcommand prints full assembled prompt to stdout with section stats on stderr
- **`weiran lint`**: Validates markdown frontmatter formats across topics, skills, and CLAUDE.md files
- **CORE.md loading**: Read-only rules file loaded before soul files, auto-restored if modified
- **Feedback auto-injection**: Scans `memory/topics/feedback_*.md`, extracts frontmatter name+description, injects as behavioral rules section
- **Dynamic content boundary**: Explicit split between static (soul/identity/tools) and dynamic (daily notes/TG/sessions) prompt sections, mirroring Claude Code's prompt caching concept
- **Launch directory capture**: Records original CWD before chdir, available for context
- **Current time injection**: Agent prompt includes `Current time` with time + day of week

### Safety & Hooks

- **CORE.md integrity guard**: Safety check auto-restores CORE.md from git HEAD if modified
- **Soul file shrinkage detection**: Warns if any protected file (SOUL/IDENTITY/USER/AGENTS/BOOT) shrinks >20% — prevents accidental content deletion during "optimization"
- **Markdown format validation**: `validateMdFormats()` checks topic/skill frontmatter and CLAUDE.md structure

### Server Mode

- **Haiku naming pool**: `server_haiku_pool.go` — pool of Haiku instances for fast session auto-naming
- **Session create refactor**: `createSessionWithOpts` replaces positional args with `sessionCreateOpts` struct
- **Control response routing**: `bridgeStdout` routes `control_response` messages to sync waiters via `deliverResponse`
- **Suppress close on reload**: `suppressClose` atomic bool prevents "Session ended" on intentional process restart (chrome toggle)
- **Resume flag passthrough**: `-r` TUI picker now passes through extra flags (e.g. `--chrome`) to the resumed session

### Web UI

- **GAL interactive components**: Choice cards, quick reply chips, star rating, image gallery — all rendered from markdown code fences (`weiran-choices`, `weiran-chips`, `weiran-rating`, `weiran-gallery`)
- **GAL replay styling**: Muted left-border + "回放" label for replayed history messages
- **Category chip filter**: Session list filterable by category chips
- **Model badge repositioning**: Moved to accommodate hamburger menu button

### Code Quality

- **Project roots**: `workspace/scripts` added to default project scan roots
- Removed unused `os/exec` import from `server_rename.go`

## v1.7.0

### Server Mode

- **Auto-rename API**: New `POST /api/sessions/{id}/auto-rename` endpoint — calls Claude CLI (`claude -p`) instead of direct Anthropic API, uses system model routing
- **User message broadcasting**: User messages now broadcast to SSE/WS subscribers, persisting across session switches
- **Resume dedup**: Resuming a Claude session that's already active returns the existing session instead of creating a duplicate
- **Resume with display name**: `POST /api/resume` accepts `name` field to preserve original session title
- **Activity tracking**: Broadcaster tracks `lastEventTime`, exposes `last_event` and `idle_seconds` in session snapshot
- **Session name lookup fix**: `readClaudeSessionName` now scans all session JSON files by `sessionId` field instead of assuming filename = UUID
- **Nested env cleanup**: `filterNestedClaudeEnv` strips `CLAUDE_CODE_SESSION` / `CLAUDE_CODE_ENTRY_POINT` from child processes

### Web UI

- User messages rendered from broadcast events (persist when switching sessions)
- `/rename` slash command: with args = manual rename, no args = auto-rename via Haiku
- History sessions already active → click selects instead of re-resuming
- Session switch cleanup (typewriter state, thinking indicator reset)
- `doResume()` refactor for direct history-to-session navigation

### Evolve

- E2E server API test (`tests/server-api-e2e.sh`) integrated into evolve workflow

### Code Quality

- Go formatting cleanup (string concatenation, struct field alignment)
- Removed direct Anthropic API dependency from auto-rename (`getAnthropicAPIKey` deleted)
- Updated tests for new `filterNestedClaudeEnv` and CLI-based rename

## v1.6.0

### Server Mode Enhancements

- **Server mode context injection**: Boot protocol now detects server mode and injects Web UI-specific environment instructions (image rendering, link previews, tool chain visibility)
- **OG link preview API**: New `GET /api/link-preview?url=` endpoint fetches Open Graph tags for URL preview cards in Web UI
- **Session name sync from Claude Code**: Auto-reads Claude Code's session metadata on init to sync session names (replaces generic `session-*` / `resume-*` prefixes)
- **Image support in history**: Parse and display base64 images from tool_result content blocks when loading session history
- **bridgeStdout onInit callback**: Split stdout bridge into `onInit` + `onResult` callbacks for cleaner session lifecycle handling

### UI

- Mobile-first Web UI improvements (from previous commits, continued iteration)

## v1.5.0

- Session rename, slash commands, typewriter streaming, launchd integration

## v1.4.0

- Mobile-first UI overhaul
- Historical message loading on session resume

## v1.3.0

- Server mode — HTTP API + web UI for persistent Claude Code sessions
- Refactored all task prompts to text/template

## v1.2.0

- Open-sourced as soul-cli
- Version management with build/rollback
- Evolve mode for daily self-iteration
