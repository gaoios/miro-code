# Codex Backend — Operations Runbook

> Round 5 deliverable. Reflects state at commit `feat/codex-backend@<HEAD>`.
> Companion docs: [`codex-backend-plan.md`](codex-backend-plan.md) (architecture)
> · [`codex-app-server.md`](codex-app-server.md) (protocol summary)

This runbook is for the operator who has to install, enable, switch on,
debug or roll back the codex backend on a running miro server.

---

## 1 — TL;DR

| Question | Answer |
|----------|--------|
| What is "codex backend"? | A second Backend implementation that drives `codex app-server` over JSON-RPC 2.0 instead of `claude` over stream-json. Same `Backend` interface, same SSE / IPC / Telegram fanout. |
| Default? | **No.** All existing sessions still spawn `claude`. Codex is opt-in per `agents.codex.enabled = true` and per-session via `backend: "codex"` or a model-prefix / `model_map` lookup. |
| Smallest experiment | `bash scripts/codex-smoke.sh` (stage 1 only — no server needed) |
| Production smoke | Same script with `MIRO_SERVER_URL` + `MIRO_AUTH_TOKEN` set |
| Where do metrics live? | `GET /api/codex/metrics` (no auth) — see §6 |
| Daily liveness check | `hooks/heartbeat.d/95-codex-self-check.sh` runs once/day at 04:00 local |

---

## 2 — Installation

### 2.1 codex CLI

```bash
npm install -g @openai/codex
which codex                        # /opt/homebrew/bin/codex on mac
codex --version                    # codex-cli 0.153.0 or newer
```

### 2.2 Authenticate

```bash
codex login
```

This populates `~/.codex/` (auth.json, config.toml, etc.). Without it
`codex app-server` will start but every `thread/start` rejects.

Verify:

```bash
ls ~/.codex/auth.json && echo OK
```

### 2.3 miro build with codex backend

The codex backend lives on `feat/codex-backend`:

```bash
cd ~/.openclaw/workspace/scripts/miro
git checkout feat/codex-backend
make            # build with ldflags + codesign
make install    # → ~/.local/bin/miro
```

**Always `make`. Never bare `go build`** — the codesign step is needed
on macOS for Keychain access (Telegram tokens etc.).

### 2.4 Restart the server

```bash
make server-restart
```

This stops the launchd `miro` job, replaces the binary, starts it
again. Existing CC sessions are rehydrated; codex sessions (none yet)
would not be (no resume implemented yet — see §8).

---

## 3 — Enabling the backend

Edit `~/.openclaw/data/config.json` (the file the server reads on
startup; `miro config` shows the resolved values):

```jsonc
{
  "agents": {
    "codex": {
      "enabled": true,
      "binary": "codex",                                  // optional, PATH lookup
      "model_map": {
        "opus[1m]":             "gpt-6-astra",             // miro name → codex name
        "codex/gpt-5.4-codex":  "gpt-5.4-codex"
      },
      "approval_policy": "never",                         // codex-side default
      "permission_profile": "workspaceWrite"               // sandbox alias, see §8.5
    }
  }
}
```

After edit:

```bash
make server-restart
```

Quick sanity check:

```bash
curl -s http://127.0.0.1:8090/api/codex/metrics | jq
```

You should see the JSON snapshot with `backend_sessions_total` empty
and `codex_handshake.ok_total = 0`. The endpoint existing at all is
proof the Round 5 binary is live.

---

## 4 — Switching a session to codex

Three ways, in priority order:

### 4.1 Explicit per-session (API)

```bash
curl -X POST http://127.0.0.1:8090/api/sessions \
  -H "Authorization: Bearer $MIRO_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name":"my-codex-test",
    "backend":"codex",
    "model":"gpt-6-astra",
    "project":"/tmp",
    "initial_message":"reply ok"
  }'
```

Strict validation: `backend` must be `cc`, `codex`, or `auto`. `codex`
without `agents.codex.enabled` returns HTTP 400.

### 4.2 spawn flag

```bash
miro spawn --bare --backend codex --model gpt-6-astra \
  --project /tmp 'reply with one word: ok'
```

`--backend auto` (default) is identical to omitting the flag — let the
model name auto-route.

### 4.3 Auto-route by model name

`resolveBackendKind` in `codex_routing.go` runs:

1. Explicit `backend: "codex"` → codex (caller wins).
2. Explicit `backend: "cc"` → cc.
3. `agents.codex.enabled=false` → cc (default).
4. Model has `codex/` prefix → codex.
5. Model is a key in `model_map` → codex.
6. Otherwise → cc.

So with the example `model_map` above:

| Model passed in | Backend | Codex-side model |
|-----------------|---------|------------------|
| `opus[1m]`      | codex   | `gpt-6-astra`    |
| `codex/gpt-5.4-codex` | codex | `gpt-5.4-codex` |
| `claude-haiku`  | cc      | n/a              |

### 4.4 Resuming

**Codex backend has no resume yet.** Round 5 leaves `set_model` and
`thread/resume` unimplemented. A miro session that ran on codex,
then is restarted, will not pick up where the codex thread left off.
The CC backend's resume flow is unchanged.

---

## 5 — Codex absent / handshake failure

What happens when codex is broken:

1. **Binary missing** (`codex` not on PATH) → `spawnCodex` returns
   `codex start: exec: "codex": executable file not found in $PATH`.
   The session create returns HTTP 500 with that message in the body.
   The session is never registered in the manager.

2. **`codex` runs but `app-server` rejects** (e.g. version too old) →
   handshake stage 1 (`initialize`) errors out, `failInit("initialize",
   …)` runs, the codex backend transitions to `error`, and a
   `codex_backend_error` SSE event is broadcast with `stage` + `reason`.

3. **`thread/start` rejected** (auth bad, model unknown) → same path,
   `failInit("thread/start", …)`. The runbook §9 has the diagnosis
   table.

4. **Codex hangs after handshake start** → 10s `codexHandshakeTimeout`
   in `codex_backend.go` fires `failInit("initialize", "context
   deadline exceeded")`. Session moves to `error`.

In all four cases, `codex_handshake.failures_total` is incremented
(see §6). No silent downgrade to CC ever happens — that would mask
misconfiguration. To fall back to cc, the operator either:

- Removes `agents.codex.enabled = true` from config and restarts, or
- Calls the API again with `backend: "cc"`, or
- Fixes the underlying codex problem.

---

## 6 — Metrics

`GET /api/codex/metrics` (no auth) returns:

```json
{
  "uptime_seconds": 3601,
  "backend_sessions_total": {
    "cc":    127,
    "codex":   3
  },
  "codex_handshake": {
    "ok_total": 3,
    "failures_total": 0
  },
  "codex_approvals": {
    "allow_total": 8,
    "deny_total": 1,
    "ask_total": 2,
    "timeout_total": 0,
    "default_total": 17
  },
  "codex_jsonrpc": {
    "calls_total": 412,
    "sum_ms": 19387,
    "mean_ms": 47.05,
    "buckets_le_ms": {
      "50":     312,
      "200":     85,
      "1000":    13,
      "5000":     2,
      "30000":    0,
      "+Inf":     0
    }
  },
  "generated_at": "2026-04-27T22:15:01Z"
}
```

Counter semantics:

| Counter | Incremented by | Meaning |
|---------|----------------|---------|
| `backend_sessions_total{kind}` | createSessionWithOpts | Every successful spawn — bumped after the backend is in `sess.process` |
| `codex_handshake.{ok,failures}_total` | runHandshake / failInit | Each thread/start completion or any of the 4 fail paths |
| `codex_approvals.{allow,deny,ask,timeout,default}_total` | handleCodexApproval | One per `UEvtApproval` after the hook chain decides |
| `codex_jsonrpc.calls_total` + buckets | CodexJSONRPCClient.Call | Per-call, only on completion (not on transport-level errors) |

There is **no** prometheus exporter. If you want Grafana, scrape the
JSON via your existing `node-exporter` or write a thin Go shim. The
shape is stable.

---

## 7 — Hook behavior — CC vs Codex

Both backends route tool approvals through the same `tool-hook` rules
in `tool-hooks.yaml`, but the wire path differs:

| Aspect | CC backend | Codex backend |
|--------|-----------|---------------|
| Hook entry | `claude` invokes `~/.claude/hooks/PreToolUse` (subprocess) | `codex_app-server` sends a JSON-RPC server request, miro receives it inside the same Go process |
| Evaluator | `tool_hook.go` PreToolUse handler runs as subprocess, reads stdin/stdout | `tool_hook.go evaluateToolHookForApproval` (in-process function — Round 4 addition) |
| Decision shape | stdout JSON `{permissionDecision: allow|deny|ask, …}` | typed `Codex{CommandExec,FileChange,Permissions}ApprovalResponse` (`accept` / `decline`) |
| Audit | `tool_hook_audit` table in `sessions.db` | **Not currently audited** (Round 5 limitation, see §8) |
| `ask_user_question` | SSE event from CC → existing /answer-question endpoint → CC stdin | SSE event → /answer-question → `sess.process.sendPermissionDecision` (Backend interface — backend-agnostic) |
| Default if no rule | bypassPermissions = allow | Same — handleCodexApproval auto-allows |
| Default on bridge dead | n/a (CC dies takes session with it) | 30s `codexApprovalWaitTimeout` → default-decline so codex thread doesn't deadlock |

Decision **semantics** match. Decision **plumbing** differs.

If you change a tool-hook YAML rule, both backends pick it up — the
codex side reads the same `loadToolHookConfig`.

---

## 8 — Known limitations (Round 5)

These will be addressed in subsequent rounds; they are not bugs.

### 8.1 No mid-thread set_model / resume

`codexBackend.controlRequest` returns an error for `set_model`. Codex
doesn't support changing the model on an existing thread; the only path
is to call `thread/resume` with a new thread, which miro does not
yet wire into the CC-style fallback chain.

If you change a session's model and that session is on codex, the
control request errors out and the SSE shows `error_response` — the
backend stays alive and on the original model.

### 8.2 No fallback chain

For CC, when a session hits 429 / token-budget-exhausted, miro can
restart it on the next entry of `defaultModelFallbacks`. The codex
backend doesn't participate in that chain yet — `watchCodexExit` flips
the session to `error` and the operator retries manually.

### 8.3 Front-end render

The Web UI doesn't have a renderer for the new `codex_*` SSE event
types (`codex_turn_started`, `codex_item_delta`, etc.). It will
display the synthesized CC-shaped `result` event correctly so basic
"did the turn finish? was there an error?" works, but per-token
streaming for codex turns isn't visualized — it shows up only in the
SSE log.

The fix is `web/dashboard/handlers.ts` — add cases that mirror the
CC `assistant` handler for the codex event names. Rough outline lives
as a TODO in `server_process_codex.go` near the SSE emission switch.

### 8.4 Tool-hook audit absent for codex

The CC stream-json hook subprocess writes to `tool_hook_audit` so we
can run weekly stats with `miro tool-hook stats --days 7`. The
in-process codex evaluator (`evaluateToolHookForApproval`) does not.
Audit-equivalent path: read `codex_approvals.*` from
`/api/codex/metrics` for aggregate counts; per-rule attribution
requires Round 6.

### 8.5 `permission_profile` maps to the `sandbox` enum

**Codex 0.153.x removed `permissionProfile` from `thread/start` and
`thread/resume`.** Sending it now fails the handshake with:

```
-32602: `permissionProfile` is no longer supported for `thread/start`;
        use `permissions` with a named profile id instead
```

We do **not** take the `permissions` route that the error suggests: it
requires the `experimentalApi` capability to be declared during
`initialize`, and returns `-32600` without it. The non-experimental
replacement is the `sandbox` field, a plain enum:

```jsonc
"workspace-write" | "read-only" | "danger-full-access"
```

The config key keeps its historical name, so existing
`agents.codex.permission_profile` values keep working. `codex_backend.go`
maps them via `codexSandboxMode`:

| config value | wire `sandbox` |
|---|---|
| `""` | *(omitted — codex default, which is read-only)* |
| `"default"` / `"workspaceWrite"` / `"workspace-write"` | `"workspace-write"` |
| `"readOnly"` / `"read-only"` | `"read-only"` |
| `"dangerFullAccess"` / `"danger-full-access"` / `"disabled"` | `"danger-full-access"` |

Anything else — including the old JSON-literal form
(`{"type":"disabled"}`), which no longer exists on the wire — **fails the
init with a clear message** rather than being silently dropped. Silently
omitting the field would fall back to codex's read-only default, producing
a session that looks alive but cannot write.

> **History.** This section previously documented the opposite: codex
> 0.125.0 turned `permissionProfile` from a bare string into a typed enum,
> and this backend emitted that object. 0.153.x then deleted the field
> outright. Because the init error was swallowed downstream, the result was
> a dispatch that reported success, exited 0, and did no work — see §9.4.

---

## 9 — Troubleshooting

### 9.1 "codex backend not enabled" (HTTP 400 on POST /api/sessions)

```
{"error":"backend codex not enabled (agents.codex.enabled = false)"}
```

Fix: edit `config.json`, `agents.codex.enabled = true`, then
`make server-restart`. Verify with `curl /api/codex/metrics`.

### 9.2 `failInit("initialize", "EOF")` after spawn

Codex died before responding. Common causes:

1. Binary not codex (e.g. an old one) — `codex --version` should be
   ≥ 0.153.0 for the current protocol (see §8.5 for the breaking change).
2. Crashed at startup. Reproduce with the smoke script — it dumps the
   last 20 lines of codex stderr on failure.

### 9.3 `failInit("thread/start", "-32602 ... permissionProfile ...")`

Codex's `thread/start` validator rejected the permission field. This is
the 0.153.x contract change — see §8.5. It should not be possible on a
current build (the backend sends `sandbox`); if you see it, you are
running a stale binary.

### 9.4 A codex spawn "succeeds" but does no work

Symptoms: `soul spawn --bare --backend codex … --wait` prints
`session completed` and exits **0**, but `server_sessions.user_turns`
stays 0 and `~/.codex/sessions` gains no entry.

This was a real bug, fixed in three places — if it recurs, check each:

1. **Backend handshake.** An init failure must set `initErr` *and* the
   session must go terminal. `watchCodexExit` maps `initErr` → status
   `error`.
2. **Message injection.** Call sites used to call `process.waitInit`
   directly and could not tell "slow" from "dead", so they wrote the first
   message into a dead process and logged the failure. They now go through
   `serverSession.awaitBackendReady`, which returns false (and marks the
   session `error`) when `alive()` is false.
3. **CLI exit code.** `/api/sessions/{id}/wait` returns HTTP 200 for every
   terminal state, including `error`, so the CLI must inspect the body.
   `spawn.go waitSession` now fails the run on `status == "error"`.

Diagnose with the server log — a handshake failure logs the raw JSON-RPC
error verbatim.

### 9.4 Approvals never resolve (turn hangs at "thinking" indefinitely)

1. Check `/api/codex/metrics` `codex_approvals.timeout_total` — if
   that's growing, the bridge is alive but the hook chain is
   indecisive (defaults to `default-decline` after 30s).
2. Check tool-hook YAML for stale `tool` matchers — the codex `toolName`
   is the codex protocol name (`commandExec`, `fileChange`,
   `permissionsRequest`), not the CC tool name. Rules written for
   CC's `Bash`/`Edit`/etc. won't fire.
3. SSE log shows `codex_hook_decision` events when a hook actually
   decided. No `codex_hook_decision` event after the `ask_user_question`
   event = the user never answered the prompt.

### 9.5 Smoke script stage 1 passes, stage 2 fails

That means the codex protocol works but the miro wiring is broken.
Check in order:

- Server logs (`tail -50 ~/.openclaw/data/server.log` or
  `journalctl --user -u miro` depending on platform).
- `/api/codex/metrics` returns 200 → server has Round 5 endpoints.
- POST /api/sessions response body — usually has the actual error.

### 9.6 Heartbeat sent a "[codex-self-check] FAILED" Telegram

The 04:00 daily probe failed. The Telegram message has the last 20
lines of stderr; the most common causes are §9.2 and §9.3. The
heartbeat hook never blocks the heartbeat itself (always exits 0) so
the `*-codex-*` failure won't cascade.

To re-run manually:

```bash
MIRO_DB=/tmp /Users/kiyor/.openclaw/workspace/scripts/miro/hooks/heartbeat.d/95-codex-self-check.sh
ls /tmp/.codex-self-check-*    # marker so it doesn't re-run today
```

(Setting `MIRO_DB=/tmp` writes the once-per-day marker to /tmp instead
of the production state dir, so the real state isn't poisoned.)

---

## 10 — Rollback

Codex backend is opt-in. To turn it off without rebuilding:

1. Edit `config.json` → `agents.codex.enabled: false`.
2. `make server-restart`.

Existing codex sessions are not migrated — they get killed when the
server restarts (the codex subprocess exits). The CC sessions go
through the normal rehydrate path.

To go further and remove the binary entirely, check out `main`:

```bash
git checkout main
make install
make server-restart
```

The CC backend behavior is identical between branches — Round 1
preserved every CC code path verbatim.

---

## 11 — Files of interest

| File | What's in it |
|------|--------------|
| `backend.go` | `Backend` interface, `BackendKind`, `BackendInfo`, `SessionOpts` |
| `unified_events.go` | `UnifiedEvent` envelope; `UEvt*` kinds |
| `codex_jsonrpc.go` | JSON-RPC 2.0 client (NDJSON over stdio); request/notification dispatch; per-call latency record |
| `codex_protocol.go` | Typed Go structs for the codex app-server protocol subset |
| `codex_backend.go` | `Backend` impl wrapping the codex subprocess; handshake; approval async pending; shutdown |
| `codex_routing.go` | `resolveBackendKind` / `isCodexModel` / `codexResolveModel` |
| `codex_metrics.go` | In-process counters + `/api/codex/metrics` handler |
| `server_process_codex.go` | Bridge from `cb.events()` → SSE; approval routing through `evaluateToolHookForApproval` |
| `tool_hook.go evaluateToolHookForApproval` | In-process PreToolUse evaluator codex shares with CC |
| `scripts/codex-smoke.sh` | This runbook's `make smoke` |
| `hooks/heartbeat.d/95-codex-self-check.sh` | Daily 04:00 liveness probe |
| `docs/codex-backend-plan.md` | Full architecture design |
| `docs/codex-app-server.md` | Protocol summary (initialize / thread / turn / item / approval) |

---

## 12 — Round 6+ open items

Tracked here for the next person who touches this code:

- [ ] `set_model` via `thread/resume` — needs codex-side thread persistence and a resumeID survives across miro restarts.
- [ ] Codex into the fallback model chain — `watchCodexExit` calls `tryNextFallback` on rate-limit.
- [ ] Front-end renderer for `codex_*` SSE events.
- [ ] `tool_hook_audit` write from `evaluateToolHookForApproval` (currently CC-only).
- [x] `permission_profile` → `sandbox` enum migration (§8.5). Values are plain aliases now; no JSON-escape needed.
- [ ] Prometheus `/metrics` endpoint exposing the codex counters in standard exposition format.
