# miro-code

**一个界面调度你所有的 AI agent，像 CEO 经营一家公司。** miro 自己不生产内容，只干三件事：**调度、分配、验收**。

1. **多 agent 并行调用** —— 在同一个操作界面里调用所有已适配的 agent 工具；每个 agent 各用各的订阅与额度，Key 全程留在本地；
2. **池子随生态扩容** —— 出了新 agent、新优惠套餐，加进配置就能被 CEO 直接调用，不换界面、不改习惯；
3. **真活并行干** —— 一条 4 分钟视频的完整工序，五个 agent 同时开工，你只做判断与验收。

> **EN TL;DR**: One interface to orchestrate all your AI agents — each with its own subscription and quota. miro-code is the CEO: it dispatches, assigns, and accepts; you judge. Fork of [kiyor/soul-cli](https://github.com/kiyor/soul-cli) — thanks to @kiyor for the excellent upstream.

📖 **中文使用教程**：[docs/miro/tutorial.zh.md](docs/miro/tutorial.zh.md) · GitBook 在线版：<https://xlegao.gitbook.io/mirobiz/>

---

## 核心卖点一：一个界面，多家 agent，各花各的额度

| 已适配 Agent | 在公司里的角色 | 额度归属 |
|---|---|---|
| **Claude Code** | CEO / 监督者：拆解任务、分配、验收 | 你的 Claude 订阅 |
| **Codex**（含 GPT Image 2 生图） | 工程实现 + 高质量封面/头图 | 你的 OpenAI 订阅 |
| **Grok** | 对抗审查、事实校验、第二实现者 | 你的 xAI 账号 |
| **Kimi** | 长文档、前端视觉、复杂任务 | 你的 Kimi 套餐 |
| **Antigravity（agy）** | Gemini 系执行与综合分析 | 你的 Google 订阅 |
| **OpenCode** | DeepSeek 系写稿、廉价批量执行 | 你的 OpenCode 订阅 |
| **即梦 CLI / MiniMax / ElevenLabs** 等 | 生图、配音等工序型工具 | 各自的平台额度 |

要点：**miro 不碰你的钱**——不转售 Key、不代充、不归集余额。每个 agent 用它自己账号的额度，你原有的订阅一分不浪费；CEO 只负责把对的活、在对的时间、派给对的 agent，并逐路验收。

## 核心卖点二：池子随时扩容

新 agent 发布了？某家出了骨折优惠套餐？**写进配置就进池子**，CEO 立即可调用——

- 对话型 agent 注册为 backend（走 `spawn --bare` 派发）；
- 工序型工具（生图、配音、部署类 CLI）注册在 `tools` 下，同一项目环境执行；
- 不用换界面、不用改操作习惯，你的调用入口永远是这一个。

## 核心卖点三：实战工作流 —— 一条 4 分钟视频，五路并行

拿「多平台订阅，谁在真省钱？」这个选题举例。你只说一句话：

> 「做一条 4 分钟产品介绍视频，选题已定。」

CEO 自动拆解工序，按各 agent 特长**并行**派发：

![miro-code 多 agent 并行视频工作流](docs/assets/workflow-parallel.png)

| 工序 | 派给谁 | 为什么是它 |
|---|---|---|
| 分析任务、拆分工序 | 任意你指定的模型 | CEO 的思考层，你说了算 |
| 写中文口播稿 | **DeepSeek**（经 opencode backend） | 中文长文案强、单价低 |
| 封面 / 头图，批量出 5 版 | **Codex · GPT Image 2** | 高质量生图，供你挑选 |
| 正文配图 16 张 | **即梦 CLI** | 额度便宜量大管饱，草稿可整批重跑 |
| 中文配音 | **MiniMax** | 定稿音色、分片生成便于局部重配 |
| 事实校验 | **Grok** | 逐句核对每个价格数字，输出修订清单 |
| 机动产能 | **OpenCode / Qoder / WorkBuddy** | 按各家配额与优惠灵活加派 |

五路同时开工，互不等待；CEO 逐路回收、挑错、不合格打回重做，最后汇总合成。**同样的活，串行做要一下午，并行做只要一轮**——而且每一环用的都是最擅长它的那家额度。

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
- 非对话型 CLI（部署、生图、平台工具类）注册在 `tools` 下——同一项目环境执行，但不冒充模型 backend，这是接入即梦 / Meoo 类工具的扩展点。

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
- Server 默认只监听本机（`127.0.0.1`），所有 API 强制鉴权

## 与上游的关系

本仓库 fork 自 [kiyor/soul-cli](https://github.com/kiyor/soul-cli)（v1.12.0 基线），在此之上维护 Miro Code 品牌的中文文档、部署实践与产品化迭代。命令层面当前仍为 `miro`（独立发行包规划中）。上游的英文文档见 <https://kiyor.github.io/soul-cli/>。

## License

MIT
