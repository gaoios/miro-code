# Codex app-server JSON-RPC 协议摘要

> 给 miro codex backend 实现用的协议参考。原文在 codex repo:
> `~/code/codex/codex-rs/app-server/README.md`（1786 行完整版）
> 本文档只保留 miro 适配需要的子集。

## 一句话

`codex app-server` 是 codex 的后端 daemon 模式，通过 JSON-RPC 2.0 暴露 thread/turn/item 三层抽象。OpenAI 自己用它驱动 VSCode 扩展。

## 启动

```bash
codex app-server --listen stdio://             # NDJSON over stdio (推荐)
codex app-server --listen unix://PATH          # unix socket (本地 IPC)
codex app-server --listen ws://127.0.0.1:8200  # websocket (experimental)
```

加 `LOG_FORMAT=json` 让 stderr 输出结构化日志。

## 协议骨架

JSON-RPC 2.0，省略 `"jsonrpc": "2.0"` 字段。每条消息一行 JSON（stdio 模式 NDJSON）。

四种消息：
- **Request**: `{ id, method, params }` — 客户端发，等响应
- **Response**: `{ id, result }` 或 `{ id, error }` — 对 request 的回应
- **Notification**: `{ method, params }` — 单向，无 id 无响应
- **Server Request**: 服务器主动发的 request（如 `*/requestApproval`），客户端必须回复

错误码标准：`-32001` = "Server overloaded"（指数退避）。其他错误用 enum：
- `ContextWindowExceeded` / `UsageLimitExceeded`
- `HttpConnectionFailed { httpStatusCode? }`
- `ResponseStreamDisconnected { httpStatusCode? }`
- `Unauthorized` / `SandboxError` / `BadRequest` / `InternalServerError` / `Other`

## 生命周期

```
1. spawn codex app-server
2. client → server: initialize { clientInfo, capabilities }
                    Response: { codexHome, platformFamily, userAgent }
3. client → server: initialized (notification, 无 id)
4. 此后才能调正常 method
```

`initialize` 之前任何调用回 `"Not initialized"` 错误。重复 `initialize` 回 `"Already initialized"`。

`capabilities.optOutNotificationMethods` 可精确屏蔽 notification 方法名（如 `["item/agentMessage/delta"]`）。

## 三大抽象

```
Thread (一段对话, 持久化)
  └── Turn (一轮 user → agent)
        └── Item (userMessage / agentMessage / commandExecution / fileChange / reasoning / ...)
```

Item 三段式生命周期：`item/started` → `item/<type>/delta`*N → `item/completed`。

## miro 适配子集（必须实现）

### 必需的 method（client → server）

| Method | 用途 | 何时调 |
|--------|------|--------|
| `initialize` | 握手 | spawn 后 |
| `initialized` | 握手确认（notification） | 收到 initialize response 后 |
| `thread/start` | 开新 thread | session 创建时 |
| `thread/resume` | 续 thread | session 重新激活时 |
| `turn/start` | 提交 user 输入开始一轮 | 每条用户消息 |
| `turn/interrupt` | 取消当前 turn | 用户按停 / session destroy |
| `thread/unsubscribe` | 取消订阅 | session 即将销毁 |

### 必需的 notification（server → client）

| Method | 含义 | 处理 |
|--------|------|------|
| `thread/started` | thread 已启动 | 记 thread.id，转 UnifiedEvent |
| `thread/status/changed` | thread 状态变化 | log，必要时切 session 状态 |
| `turn/started` | turn 开始 | TurnStarted UnifiedEvent |
| `turn/completed` | turn 结束（含 status: completed/interrupted/failed） | TurnCompleted UnifiedEvent |
| `item/started` | item 开始 | ItemStarted UnifiedEvent |
| `item/agentMessage/delta` | agent 文本 chunk | ItemDelta(text) |
| `item/reasoning/summaryTextDelta` | reasoning 文本 chunk | ItemDelta(reasoning) |
| `item/commandExecution/outputDelta` | 命令输出 chunk | ItemDelta(output) |
| `item/completed` | item 结束（最终状态权威） | ItemCompleted UnifiedEvent |
| `error` | turn 中错误 | log，传错给前端 |

### 必需处理的 server request（server → client，要回复）

| Method | 含义 | 回复 |
|--------|------|------|
| `item/commandExecution/requestApproval` | 命令执行批准请求 | `{ decision: "accept" \| "decline" \| "cancel" \| "acceptForSession" }` |
| `item/fileChange/requestApproval` | 文件改动批准请求 | 同上 |
| `item/permissions/requestApproval` | 权限请求 | `{ scope: "turn" \| "session", permissions: {...} }` |

miro 通过 tool-hook 桥接：把这些 request 翻译成 PreToolUse hook 事件喂现有规则。

## 关键 schema 例子

### initialize Request

```json
{
  "id": 0,
  "method": "initialize",
  "params": {
    "clientInfo": {
      "name": "miro",
      "title": "miro soul-cli",
      "version": "1.12.0"
    },
    "capabilities": {
      "experimentalApi": false,
      "optOutNotificationMethods": ["item/agentMessage/delta"]
    }
  }
}
```

Response:
```json
{
  "id": 0,
  "result": {
    "userAgent": "codex/0.x.x",
    "codexHome": "/Users/kiyor/.codex",
    "platformFamily": "unix",
    "platformOs": "darwin"
  }
}
```

### thread/start

```json
{
  "id": 10,
  "method": "thread/start",
  "params": {
    "model": "gpt-5.1-codex",
    "cwd": "/Users/kiyor/.openclaw/workspace",
    "approvalPolicy": "never",
    "sandbox": "workspace-write"
  }
}
```

Response:
```json
{
  "id": 10,
  "result": {
    "thread": {
      "id": "thr_abc",
      "modelProvider": "openai",
      "createdAt": 1730910000
    }
  }
}
```

随后服务器主动发 `thread/started` notification。

### turn/start

```json
{
  "id": 30,
  "method": "turn/start",
  "params": {
    "threadId": "thr_abc",
    "input": [
      { "type": "text", "text": "Hello" }
    ]
  }
}
```

Response:
```json
{
  "id": 30,
  "result": {
    "turn": {
      "id": "turn_456",
      "status": "inProgress",
      "items": [],
      "error": null
    }
  }
}
```

### item/agentMessage/delta notification（流式文本）

```json
{
  "method": "item/agentMessage/delta",
  "params": {
    "threadId": "thr_abc",
    "turnId": "turn_456",
    "itemId": "item_789",
    "delta": "Hi"
  }
}
```

### turn/completed notification

```json
{
  "method": "turn/completed",
  "params": {
    "turn": {
      "id": "turn_456",
      "status": "completed",
      "items": [],
      "error": null
    }
  }
}
```

注意：当前协议的 `turn.items` 在 completion 通知里是空数组。要拼最终内容必须靠 `item/*` notification 流。

### Approval Server Request 示例

```json
// server → client
{
  "id": 60,
  "method": "item/commandExecution/requestApproval",
  "params": {
    "threadId": "thr_abc",
    "turnId": "turn_456",
    "itemId": "call_123",
    "command": ["rm", "-rf", "/tmp/foo"],
    "cwd": "/Users/kiyor",
    "reason": "agent wants to clean tmp",
    "commandActions": [...],
    "availableDecisions": ["accept", "decline", "cancel"]
  }
}

// client → server
{
  "id": 60,
  "result": {
    "decision": "decline"
  }
}
```

## Schema 自动生成

```bash
codex app-server generate-ts --out tmp/codex-schema-ts
codex app-server generate-json-schema --out tmp/codex-schema-json
```

输出固定到当前 codex 版本。miro 的 codex_protocol.go 应该从这里反转 Go 结构（手写最常用 40-50 个，其他需要时再加）。

## Authentication / Sandbox 选项

> ⚠️ **0.153.x 契约变更**：`thread/start` / `thread/resume` 的 `permissionProfile`（typed
> 对象）已被移除，换成 **`sandbox`** 枚举。继续发旧字段会得到
> `-32602: permissionProfile is no longer supported for thread/start;
> use permissions with a named profile id instead` —— 且因为是握手阶段失败，
> 后端必须把 init 错误传播成 session 失败，否则会伪装成"成功但没干活"。

- `sandbox`: `"read-only"` / `"workspace-write"` / `"danger-full-access"`（推荐字段）
- `approvalPolicy`: `"never"` / `"unlessTrusted"` / `"untrusted"` / `"onFailure"`
- `permissions`: 具名 profile id 字符串。**不要用** —— 它要求握手时声明
  `experimentalApi` capability，否则报 `-32600`。soul 走 `sandbox`。
- ~~`permissionProfile`~~: 0.153.x 起已移除
- ~~`sandboxPolicy`~~: 已 deprecate（仅 `turn/start` 上还留着，soul 从不发送）

miro 默认配置建议：`permission_profile: "workspaceWrite"`（config key 名保留，
在线上映射为 `sandbox: "workspace-write"`）+ `approvalPolicy: "never"`（让 hook 全权管控）。

## 与 CC stream-json 对比速查

| 维度 | CC stream-json | Codex app-server |
|------|---------------|-----------------|
| 协议 | 私有 NDJSON | JSON-RPC 2.0 |
| Schema 生成 | 没有 | `generate-ts` / `generate-json-schema` |
| 多 transport | 只 stdio | stdio / unix / ws |
| 健康探针 | 无 | `/readyz` / `/healthz` |
| 认证 | 无 | capability-token / signed-bearer |
| 背压 | 无 | bounded queue + `-32001` |
| Approval | hook 配置（异步） | server-initiated request（同步 reply） |
| Resume | `--resume <id>` 重启进程 | `thread/resume` 不重启 |
| Plan / Diff aggregate | 无 | `turn/plan/updated` / `turn/diff/updated` |
| Inject context | 拼 prompt | `thread/inject_items` |
| Compact | 看不见 | `thread/compact/start` 可控 |
| Schema 类型 | 文档 + 猜 | typed enum |
| 错误分类 | free-text | `CodexErrorInfo` enum |

## Codex 二进制要求

- 最低版本：?（待主人确认装的是哪个版本）
- 安装：`brew install codex` 或 `npm i -g @openai/codex`
- 检查：`codex --version`
- 第一次跑：需要 `codex auth` 完成 OAuth（OpenAI ChatGPT 订阅）

> ⚠️ **认证体系完全独立**：codex 走 OpenAI 自己的 token，不是 Anthropic OAuth。claude-pool / GoodVision 整个生态在这条路径上不可用。这是设计上的边界，不是 bug。
