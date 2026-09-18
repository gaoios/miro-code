# Codex Backend 适配计划

> Branch: `feat/codex-backend`
> 起点: 2026-04-27
> 目标: 让 soul-cli (miro) 能用 OpenAI codex 的 `app-server` JSON-RPC 协议作为备份心脏，与现有 Claude Code stream-json 心脏并存。

## 背景

- 当前心脏 = `claude -p --input-format stream-json --output-format stream-json` 长进程，schema 是 CC 私有协议
- Anthropic 已禁第三方 OAuth API（2026-04），需要"生态层级"的 fallback
- Codex 的 `app-server` 是 JSON-RPC 2.0 工业级 RPC（stdio / unix / ws transport，三种），typed schema 可生成，比 stream-json 稳定且可观测
- 协议摘要见 `docs/codex-app-server.md`（本计划同期产出）

## 不做什么（防止 scope 漂移）

- 不替换 CC 心脏。CC 仍是默认主力，codex 是**并存**的可选 backend
- 不重做 claude-pool / OAuth 池。codex 走自己的认证体系，号池是另一个项目
- 不动前端协议。SSE 出口对浏览器/Telegram 透明，所有适配压在 server 内部
- 不实现 codex 独有 feature（realtime / WebRTC / windows sandbox）

## 设计原则

1. **Backend 抽象**：抽 `Backend` interface，CC 实现叫 `claudeBackend`，codex 实现叫 `codexBackend`，session 持有 `Backend` 而非 `*claudeProcess`
2. **Unified Event Bus**：定义 `UnifiedItem` / `UnifiedTurnEvent` 中间层，两个 backend 各自往这层翻译；前端 SSE 从这层 fan-out
3. **Per-session 选择**：session 创建时一个 `backend` 字段（默认 `cc`，可 `codex`），中途不切
4. **测试可注入**：Backend 接 fake，单测不需要真心脏

## 架构图

```
┌───────────────────────────────────────────────────────┐
│  Web UI / Telegram / IPC peer                         │
└─────────────────┬─────────────────────────────────────┘
                  │ SSE / WS / IPC（不变）
┌─────────────────▼─────────────────────────────────────┐
│  server_session.go  (state machine, TTL, IPC, prompt) │
│      ┌──────────────────────────────────────┐         │
│      │  UnifiedEventBus  (新增)             │         │
│      └────┬───────────────────┬─────────────┘         │
│           ▲                   ▲                       │
│  ┌────────┴────────┐  ┌───────┴────────┐              │
│  │ claudeBackend   │  │ codexBackend   │              │
│  │ (server_process │  │ (新增 server_  │              │
│  │  现有逻辑提纯)  │  │  process_codex)│              │
│  └────────┬────────┘  └───────┬────────┘              │
└───────────┼───────────────────┼───────────────────────┘
            │ stream-json       │ JSON-RPC 2.0 over stdio
            ▼                   ▼
       claude (CC)           codex app-server
```

## Phase / 路线图

| Phase | 主题 | 工时估 | 产物 |
|-------|------|-------|------|
| 0 | 准备 | 0.5d | 本计划 + codex 协议摘要 |
| 1 | Backend 抽象层 | 1.5d | `backend.go` interface + claudeBackend 重构通过现有测试 |
| 2 | Unified event schema | 1d | `unified_events.go` + CC 翻译层 + 现有 SSE 不回归 |
| 3 | Codex 适配（核心） | 3d | `codex_backend.go` JSON-RPC client + initialize + thread/start + turn/start + item/* 流 |
| 4 | Approval / tool-hook 桥 | 1.5d | `*/requestApproval` ↔ 现有 hook chain；保持 PreToolUse/PostToolUse 行为 |
| 5 | 配置 / 路由 | 1d | session 创建参数 + config 字段 + 模型映射表 |
| 6 | 测试 + 烟测 | 2d | 单测覆盖 + 真 codex 跑通一轮 + 心跳冒烟 |
| 7 | 观测 / 切换文档 | 0.5d | metrics + 切换 runbook |

合计 **~10.5 工程日**。relay 5 轮 × 2 工作日/轮 = 与 8-10 工程日吻合，留 1-2 日做集成测试和文档。

## 详细分解

### Phase 1 — Backend 抽象

**文件**：
- `backend.go` (新, ~150 行) — `Backend` interface
- `server_process.go` (重构, -100 行 +50 行) — 把 `*claudeProcess` 改成 `claudeBackend` 实现 `Backend`
- `server_session.go` (refactor 调用点)

**接口草案**：
```go
type Backend interface {
    Start(ctx context.Context, opts SessionOpts) error
    SendUserTurn(ctx context.Context, input []InputItem) error
    Interrupt(ctx context.Context) error
    Resume(ctx context.Context, sessionID string) error
    Events() <-chan UnifiedEvent
    Close() error
    Info() BackendInfo // kind: "cc" | "codex", model, sessionID
}
```

**约束**：
- 现有 `claudeProcess` struct 字段 100% 内部化，不暴露到 session 层
- 所有 `*claudeProcess` 引用替换为 `Backend`
- `spawnClaude` 改名 `spawnCC`，被 `claudeBackend.Start` 调用
- 现有测试**全部继续通过**（没有行为变化）

### Phase 2 — Unified Event Schema

**文件**：
- `unified_events.go` (新, ~200 行)
- `server_stream.go` (改造, ~100 行 diff)

**事件清单**（按必要性排）：
- `TurnStarted{turnID}`
- `TurnCompleted{turnID, status, error?}`
- `ItemStarted{itemID, kind, ...}` — kind: `agent_message` / `command_exec` / `tool_call` / `file_change` / `reasoning`
- `ItemDelta{itemID, deltaType, payload}` — deltaType: `text` / `output` / `summary` / `plan`
- `ItemCompleted{itemID, finalState}`
- `Approval{type, payload, replyChan}` — server-initiated approval

**翻译**：
- CC stream-json `assistant.content[]` → `ItemStarted` + `ItemDelta` + `ItemCompleted`
- CC `result` → `TurnCompleted`
- CC `tool_use` → `ItemStarted{kind:"tool_call"}`，CC tool_result → `ItemCompleted`

### Phase 3 — Codex 适配（核心）

**文件**：
- `codex_backend.go` (新, ~600 行)
- `codex_jsonrpc.go` (新, ~250 行) — JSON-RPC 2.0 framing helper
- `codex_protocol.go` (新, ~400 行) — Request/Notification typed structs

**实现顺序**（每子项约半天）：

1. `codex_jsonrpc.go`：`Client{conn}`，`Call(method, params) (result, err)`，`Notify(method, params)`，`OnRequest(handler)`，`OnNotification(handler)`，bounded queue
2. `codex_protocol.go`：从 `codex app-server generate-ts` 反转出 Go 结构（精选 40-50 个最常用类型）
3. `codex_backend.Start()`：spawn `codex app-server --listen stdio://`，发 `initialize`，等回复，发 `initialized` notification，调 `thread/start` 拿 thread.id
4. `codex_backend.SendUserTurn()`：调 `turn/start`，监听 `item/*` notification 翻成 UnifiedEvent
5. `codex_backend.Resume()`：调 `thread/resume`
6. 心跳：定期调 `thread/loaded/list` 验活

### Phase 4 — Approval / Tool-Hook 桥

CC 的 hook 模型 vs codex 的 server-initiated request：

| CC | Codex |
|----|-------|
| `PreToolUse` hook (settings.json) → 拦截器 | `*/requestApproval` request → client reply |
| `PostToolUse` hook → notification | `item/completed` notification |

策略：
- 现有 `tool-hooks.yaml` **零改动**
- codex backend 收到 `*/requestApproval` 时，模拟 PreToolUse 事件喂给现有 hook chain，根据 hook 输出生成 decision
- `item/completed` 生成 PostToolUse 事件喂 hook chain（fire-and-forget）

### Phase 5 — 配置 / 路由

**新增 config（`~/.config/miro/config.json`）**：
```json
{
  "backends": {
    "default": "cc",
    "codex": {
      "binary": "codex",
      "model_map": {
        "opus[1m]": "gpt-5.1-codex-max",
        "sonnet": "gpt-5.1-codex",
        "haiku": "gpt-5.1-codex-mini"
      }
    }
  }
}
```

**session 创建 API 加字段**：
```json
POST /api/sessions
{
  "name": "...",
  "model": "...",
  "backend": "codex"  // 新增，可选，默认 "cc"
}
```

**miro spawn**：`miro spawn --backend codex "task..."`

### Phase 6 — 测试 + 烟测

- **单测**：`codex_jsonrpc_test.go` (mock conn)、`codex_backend_test.go` (mock client)、`unified_events_test.go`（CC translator round-trip）
- **集成测试脚本**：`scripts/codex-smoke.sh` — 启动真 codex app-server，跑一个 thread/start + turn/start + 验证 item/agentMessage/delta 流
- **回归**：`go test ./...` 全绿
- **真跑一轮**：`miro spawn --backend codex "say hi in one word"` 跑通

### Phase 7 — 观测 / Runbook

- metrics 加 `miro_backend_kind`（label: cc/codex）+ `miro_codex_jsonrpc_duration_ms`
- `docs/codex-backend-runbook.md` — 怎么切换、怎么排错、codex CLI 没装时降级路径

## 风险 & 缓解

| 风险 | 缓解 |
|------|------|
| Codex JSON-RPC schema 变动（产品快迭代） | 每次升级跑 `codex app-server generate-ts` 对比 diff，schema 进 git，CI 失败先发现 |
| 在主人没装 codex 二进制时把 CC 也搞坏 | Backend 注册表 lazy 初始化，codex backend 失败 fallback 到 cc 并 warn |
| Approval 模型语义错位 | Phase 4 写一份对照表 fixture，每个 PreToolUse decision 对应 codex 的哪种 reply，单测覆盖 |
| 现有测试因 backend 抽象崩 | Phase 1 做完先全跑一次，不动行为只换 wrapper |

## 验收标准（Phase 6 结束）

- [ ] `go test ./...` 全绿（包括新增测试）
- [ ] `miro spawn --backend codex "say hi"` 能启动 → 收到 agent 回复 → session 正常 destroy
- [ ] 心跳脚本 spawn 一个 codex backend session 能完成一轮巡检
- [ ] CC backend 行为零回归（同样的 spawn 命令，输出和现在一致）
- [ ] Telegram bot 收到 codex backend session 的回复正常（SSE 透明性）
- [ ] `tool-hooks.yaml` 中的现有规则在 codex backend 下也生效

## Relay 接力安排

5 轮 × opus[1m]，每轮覆盖 1-2 个 phase：

| 轮 | 主题 | Phase |
|----|------|-------|
| 1 | Backend 抽象 + Unified Events 草案 | 1 + 2 草案 |
| 2 | Codex JSON-RPC client + protocol structs | 3 子项 1+2 |
| 3 | Codex backend Start/SendTurn/Resume | 3 子项 3+4+5 |
| 4 | Approval/hook 桥 + 配置 + 模型映射 | 4 + 5 |
| 5 | 测试 + 烟测 + runbook | 6 + 7 |

Relay state file: `memory/relay/codex-backend-20260427.json`
Originator: 当前 session（4a79b0e main commit 时的对话，记录到 briefing）
