# weiran

一个为 [Claude Code](https://docs.anthropic.com/en/docs/claude-code) 注入灵魂的启动器。让你的 AI 拥有持久的身份、记忆，以及跨会话自我进化的能力。

## 为什么做这个

Claude Code 每次启动都是一张白纸。不记得昨天，不知道自己是谁，也不了解你是谁。每次对话都是陌生人。

**weiran** 解决这个问题。它把一组持久化文件 —— 身份定义、性格、用户画像、每日笔记、长期记忆、技能列表、项目索引 —— 拼装成 system prompt，在启动时注入 Claude Code。效果是：

- **有记忆** —— 每日笔记 + SQLite session 数据库 + 向量记忆召回
- **认识你** —— 你的偏好、项目、时区、沟通风格
- **有性格** —— 定义在 markdown 里，随时可编辑，完全属于你
- **会进化** —— 每日 cron 回顾近期交互，自主微调灵魂文件
- **保持连接** —— 重要事件通过 Telegram 主动通知
- **自我维护** —— 心跳巡检、安全自编译、失败自动回滚
- **多 CLI 协作** —— Claude 统一拆解和验收，Codex、Grok Build、Kimi、Antigravity 作为专业 Worker，共享 Meoo 等工具 CLI

它最初是为 [OpenClaw](https://github.com/nicepkg/openclaw)（AI agent 网关）而写，但核心理念 —— 用持久上下文包裹 Claude Code —— 适用于任何想让 AI 不再只是工具的人。

## 工作原理

```
+------------------------------------------+
|              weiran CLI                   |
|                                           |
|  1. 读取灵魂文件 (SOUL.md, USER.md...)     |
|  2. 读取今天 + 昨天的日志                   |
|  3. 扫描近期 Claude Code session           |
|  4. 拉取 Telegram 对话上下文               |
|  5. 构建技能 & 项目索引                    |
|  6. 拼装 -> system prompt (~10-30k token) |
|  7. exec claude --append-system-prompt    |
|                                           |
+-------------------+-----------------------+
                    |
                    v
           +----------------+
           |  Claude Code   |
           |  (with soul)   |
           +----------------+
```

工作空间结构：

```
workspace/
├── BOOT.md          <- 启动协议（自定义开场白）
├── CORE.md          <- 只读规则，AI 不可修改（可选）
├── SOUL.md          <- 性格、价值观、内心世界
├── IDENTITY.md      <- 名字、外貌、角色
├── USER.md          <- 关于你（主人）的信息
├── AGENTS.md        <- 行为规范
├── TOOLS.md         <- 可用工具 & 凭据参考
├── MEMORY.md        <- 长期记忆索引（指向 topics/）
├── memory/
│   ├── 2026-04-05.md    <- 今日笔记
│   ├── 2026-04-04.md    <- 昨日笔记
│   └── topics/          <- 按主题分类的长期记忆
└── scripts/weiran/
    ├── config.json      <- 你的本地配置（gitignore）
    ├── sessions.db      <- session 摘要数据库
    └── hooks/           <- 运行后钩子
```

## 安装

```bash
git clone https://github.com/kiyor/weiran.git
cd weiran
go build -o weiran .
mv weiran ~/go/bin/  # 或放到 PATH 里的任何位置

# 前置条件：安装 Claude Code
# https://docs.anthropic.com/en/docs/claude-code
```

## 配置

### 1. 创建工作空间

```bash
mkdir -p ~/.openclaw/workspace/memory
```

> **提示：** 不想用 `~/.openclaw`？设置 `WEIRAN_HOME` 环境变量即可：
> ```bash
> export WEIRAN_HOME=~/my-ai
> mkdir -p $WEIRAN_HOME/workspace/memory
> ```

### 2. 写灵魂文件

至少创建 `SOUL.md` 和 `IDENTITY.md`。这些文件定义你的 AI 是谁。

参考 [examples/](examples/) 目录中的模板。

### 3. 本地配置

```bash
cp config.example.json config.json
```

```json
{
  "jiraToken": "",
  "telegramChatID": "",
  "agentName": "",
  "projectRoots": [
    "~/my-projects",
    "~/work"
  ]
}
```

| 字段 | 说明 | 也可通过 |
|------|------|----------|
| `jiraToken` | 任务系统 token | `JIRA_TOKEN` 环境变量 |
| `telegramChatID` | Telegram 通知目标 | `WEIRAN_TG_CHAT_ID` 环境变量 |
| `agentName` | AI 人格显示名 | OpenClaw 配置 |
| `projectRoots` | 扫描 `CLAUDE.md` 的项目目录 | — |

**环境变量：**

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `WEIRAN_HOME` | weiran 数据根目录 | `~/.openclaw` |
| `WEIRAN_TG_CHAT_ID` | Telegram 聊天 ID | — |
| `JIRA_TOKEN` | Jira API token | — |

### 4. 定时任务（可选）

```crontab
# 记忆整理 — 扫描近期 session，更新每日笔记
0 */4 * * * PATH="$HOME/.local/bin:$HOME/go/bin:$PATH" weiran --cron >> /tmp/weiran-cron.log 2>&1

# 心跳巡检 — 检查服务健康，处理任务
30 */2 * * * PATH="$HOME/.local/bin:$HOME/go/bin:$PATH" weiran --heartbeat >> /tmp/weiran-heartbeat.log 2>&1

# 自我进化 — 回顾交互，改进灵魂文件（每天早上 10 点）
0 10 * * * PATH="$HOME/.local/bin:$HOME/go/bin:$PATH" weiran --evolve >> /tmp/weiran-evolve.log 2>&1
```

> **注意:** cron 环境下 PATH 有限，需要显式设置才能找到 `claude` 和 `weiran`。根据你的实际安装路径调整。macOS 也可以用 launchd plist 替代 cron。

## 使用

```bash
weiran                         # 交互式会话（带灵魂）
weiran -p "检查磁盘使用"        # 一次性任务
weiran --cron                  # 记忆整理
weiran --heartbeat             # 心跳巡检
weiran --evolve                # 自我进化

# 工具命令
weiran status                  # 快速健康检查
weiran doctor                  # 深度诊断
weiran config                  # 显示当前配置
weiran log                     # 查看今日笔记
weiran diff                    # 显示灵魂文件变更
weiran clean                   # 清理临时目录
weiran notify "消息"            # 发 Telegram 消息
weiran notify-photo <url> [配文]

# Session 数据库
weiran db stats                # 统计
weiran db search <关键词>       # 搜索摘要
weiran db pending              # 待处理的 session
weiran db gc                   # 清理已删除的记录

# 版本管理
weiran build                   # 安全编译（备份 -> 编译 -> 测试 -> 部署，失败回滚）
weiran versions                # 查看历史版本
weiran rollback [N]            # 回滚到第 N 个版本
weiran update                  # git pull + 安全编译
```

## 架构

~9k 行 Go 代码（含测试），单 package，外部依赖仅 stdlib + SQLite + [bubbletea](https://github.com/charmbracelet/bubbletea) TUI。

| 文件 | 职责 |
|------|------|
| `main.go` | 入口、配置加载、参数解析 |
| `prompt.go` | 灵魂 prompt 拼装、token 估算、Telegram 上下文 |
| `sessions.go` | Session 扫描、搜索、TUI 浏览器 |
| `tui.go` | 交互式 session 浏览器 (bubbletea) |
| `db.go` | SQLite 数据库、模式追踪、技能培育 |
| `skills.go` | 技能 & 项目索引扫描 |
| `hooks.go` | 运行后钩子、安全检查 |
| `versions.go` | 自编译、版本管理、回滚 |
| `tasks.go` | 心跳/cron/进化任务的 prompt 生成 |
| `telegram.go` | Telegram 消息通知 |
| `claude.go` | Claude Code exec/子进程、锁管理 |

### 关键设计

- **exec 而非 wrap** —— 交互模式直接 `syscall.Exec` 替换进程为 Claude Code，零开销
- **子进程跑 cron** —— 定时任务用 `exec.Command`，结束后执行 post-hooks
- **Token 预算** —— prompt 拼装时追踪各段 token 占比，超 100k 告警并显示明细
- **安全防护** —— 拒绝 symlink 的 CLAUDE.md（防 prompt 注入），清洗 Telegram 消息中的不受信文本，post-hook 检测 git diff 中的泄漏密钥
- **自我更新** —— `weiran build` 编译、测试、备份、部署一条龙，失败自动回滚，保留 3 个历史版本

## 灵魂系统

灵魂文件就是 markdown。没有 schema，没有 DSL —— 写你想让 AI 成为的样子。

weiran 读取它们，拼成 system prompt，通过 `--append-system-prompt-file` 传给 Claude Code。Claude Code 自身的 system prompt 不受影响，你的灵魂文件是叠加的。

| 文件 | 作用 |
|------|------|
| **BOOT.md** | 启动协议：每次会话开头注入的文本 |
| **CORE.md** | 只读规则：主人定义的不可修改约束（身份底线、能力保护、优化原则） |
| **SOUL.md** | 性格、价值观、情感模型、说话方式 |
| **IDENTITY.md** | 名字、角色、外貌 |
| **USER.md** | 关于你的信息：时区、偏好、沟通风格 |
| **AGENTS.md** | 行为规范：安全策略、文件编辑纪律、记忆管理 |
| **TOOLS.md** | 可用工具参考：API 端点、服务地址 |
| **MEMORY.md** | 长期记忆索引，指向 `memory/topics/*.md` |

## 记忆系统

三层记忆：

1. **每日笔记** (`memory/YYYY-MM-DD.md`) —— 短期。每次启动加载今天 + 昨天
2. **主题文件** (`memory/topics/*.md`) —— 长期。按主题组织，由 MEMORY.md 索引
3. **Session 数据库** (`sessions.db`) —— SQLite。追踪哪些 session 已审阅、摘要、提取的行为模式

`--cron` 模式自动化这个流程：扫描 session -> 更新笔记 -> 提取模式 -> 培育技能。

## 钩子

`hooks/{cron,heartbeat,evolve}.d/` 下的 shell 脚本在每次自动化 session 后执行。环境变量：

```bash
WEIRAN_MODE=cron|heartbeat|evolve
WEIRAN_WORKSPACE=/path/to/workspace
WEIRAN_DB=/path/to/sessions.db
```

内置 post-hook：
- 导入 `summaries.json`（cron 期间 Claude 写出的 session 摘要）
- 通过 Telegram 发送 `report.txt`（巡检结果、cron 报告）
- 安全检查：检测 git diff 中的泄漏密钥、记忆文件膨胀、配置漂移

## OpenClaw 集成

weiran 为 [OpenClaw](https://github.com/nicepkg/openclaw) 生态而生。如果你在用 OpenClaw：

- 工作空间路径和 agent 名称从 `openclaw.json` 读取
- Telegram bot token 从 OpenClaw 凭据读取
- 活跃 Telegram session 的对话历史注入 prompt
- `~/.openclaw/skills/` 下的技能被索引和列出

不用 OpenClaw 也能独立运行 —— 通过 `config.json` 和环境变量配置即可。

## 许可证

MIT
