# miro-code

**给 Claude Code 装上记忆与编排能力的启动器。** 一个二进制，同时做两件事：

1. **身份与记忆注入** —— 把 Markdown 文件（人格 / 主人信息 / 记忆 / 技能）组装进系统提示词，让每次冷启动的 Claude Code 记得你是谁、昨天干过什么；
2. **多 CLI 任务编排** —— 一个入口把编码任务派发给多个已登录的编码 CLI（Codex / Grok / Kimi / Antigravity / OpenCode），Claude 当监督者，你只做判断与验收。

> **EN TL;DR**: A launcher for Claude Code with persistent identity, memory, and multi-CLI task orchestration. Fork of [kiyor/soul-cli](https://github.com/kiyor/soul-cli) — thanks to @kiyor for the excellent upstream.

📖 **中文使用教程**：[docs/miro/tutorial.zh.md](docs/miro/tutorial.zh.md) · GitBook 在线版：<https://xlegao.gitbook.io/mirobiz/>

---

## 它解决什么问题

Claude Code 每次启动都是一张白纸：不知道你是谁、没有历史、没有行事风格。miro-code 在启动时把四个 Markdown 文件组装成系统提示词注入：

```
Soul 文件 + 记忆 + 技能  →  miro  →  Claude Code（带着灵魂干活）
     (markdown)          (组装注入)   (--append-system-prompt)
```

| 文件 | 作用 |
|---|---|
| `SOUL.md` | 人格、价值观、说话风格 |
| `USER.md` | 你的时区、偏好、专业背景 |
| `MEMORY.md` + 每日笔记 | 长期记忆索引 + 最近发生了什么 |
| `AGENTS.md` | 行为规则与护栏（哪些事先斩后奏、哪些必须先问） |

二进制名即身份：编译成 `miro`、`jarvis` 或 `atlas`，所有路径、环境变量、日志随之隔离——一台机器跑多个独立 AI 互不干扰。

## 快速开始

**前置**：Go 1.25+、[Claude Code](https://docs.anthropic.com/en/docs/claude-code) 已登录。

```bash
git clone https://github.com/gaoios/miro-code.git && cd miro-code
go build -o miro .
mv miro ~/go/bin/

miro init                          # 交互式向导：起工作区、生成 soul 文件
miro                               # 启动 —— 这次它记得你
```

**不想回答问话？全参数一次到位**（脚本 / AI 自动化部署友好）：

```bash
miro init --archetype engineer --name kuro --owner alex --tz Asia/Shanghai --yes
```

### 人格原型

| 原型 | 风格 |
|---|---|
| `companion` | 情绪在场，记得小事，读得懂气氛 |
| `engineer` | 技术同侪，代码优先，干燥幽默 |
| `steward` | 运营管家，主动、有条理、安静可靠 |
| `mentor` | 耐心老师，苏格拉底式提问 |
| 自定义 | 用关键词描述，init 帮你生成 |

首次对话后，AI 会根据你的实际说话方式自动充实人格（day-0 自我丰富）。

## 多 CLI 任务编排（`spawn --bare`）

`miro server` 运行时，同一个会话 API 和 Web UI 后面可以挂多个编码 CLI。Claude 是默认监督者，worker 只接有界任务：

```bash
miro server    # 常驻：HTTP API + Web UI

# 派发：指定执行者、模型、项目路径、任务、验收方式
miro spawn --bare --backend codex --model gpt-6-astra \
  --project /absolute/path/to/repo \
  "实现 XX 并补测试" --wait

# 独立审查（换一个执行者，保持独立性）
miro spawn --bare --backend grok --model grok-4.6 --project "$PWD" "对抗性审查上一个实现" --wait
```

**可用 backend**：`codex` / `grok` / `kimi` / `agy`（Antigravity）/ `opencode`，`cc` 留给 Claude 自身。

- `--bare` + `--model` **缺一不可**：`--bare` 确保任务真正派给外部 CLI（不带时会跑 Claude 换皮）；`--model` 用该 CLI 的原生模型名，传错会在会话阶段报 `backend error`。
- 非对话型 CLI（部署、生图、平台工具类）注册在 `tools` 下——同一项目环境执行，但不冒充模型 backend，这是接入 Meoo / 即梦类工具的扩展点。

## Server 模式

```bash
miro server --token <secret>     # HTTP API + Web UI（PWA，可加到手机主屏）
```

- **Web UI**：会话列表、实时流式输出、任务进度、SQLite 会话库全文检索
- **Telegram**：通知、日报、把对话上下文注入 Telegram 会话
- **多 agent**：一个 codebase 多个二进制，数据完全隔离
- **IPC**：会话之间可以互发消息、读写、等待与关闭

## 自动化

```bash
miro --cron        # 记忆整理（定时跑，把每日笔记蒸馏进长期主题）
miro --heartbeat   # 健康巡逻（探活、报告异常）
miro --evolve      # 自我改进（复盘近期会话，修正 soul 文件，带自动回滚）
```

配合 cron/systemd 定时触发即可；`--evolve` 的每次修改都有保护机制兜底。

## 安全设计

- 拒绝符号链接写入、密钥泄漏检测
- 核心人格文件（CORE）保护，`--evolve` 改动可回滚
- spawn 的任务对 worker 是**有界**的：指定项目路径、指定任务、指定验收标准

## 与上游的关系

本仓库 fork 自 [kiyor/soul-cli](https://github.com/kiyor/soul-cli)（v1.12.0 基线），在此之上维护 Miro Code 品牌的中文文档、部署实践与产品化迭代。命令层面当前仍为 `miro`（独立发行包规划中）。上游的英文文档见 <https://kiyor.github.io/soul-cli/>。

## License

MIT
