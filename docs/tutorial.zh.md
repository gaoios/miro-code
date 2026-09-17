# miro-code 使用教程

> miro-code（上游 soul-cli）= 给 Claude Code 注入持久身份、记忆与技能的启动器，并内置多 CLI worker 编排：一条命令把编码任务派给你本机已登录的其他编码 CLI。
> 本教程 15 分钟走通：安装 → 初始化 → 日常使用 → 派发第一个跨 CLI 任务。

## 0. 你需要准备什么

| 依赖 | 说明 |
|---|---|
| Go 1.25+ | 编译用；`go version` 确认 |
| Claude Code | 已安装并登录（`claude --version` 能出版本号） |
| （可选）其他编码 CLI | codex / grok / kimi / agy / opencode 任意一个已登录，才能派发给它 |

Key 与凭据全部留在你本机，miro-code 不托管、不上传任何账号信息。

## 1. 安装（3 分钟）

```bash
git clone https://github.com/gaoios/miro-code.git
cd miro-code
go build -o miro .
mv miro ~/go/bin/        # 或放到任意 PATH 目录
miro version             # 应输出 miro x.x.x
```

> Windows / 未装 Go？见文末「常见问题」。

## 2. 初始化：创建你的 workspace（3 分钟）

```bash
miro init                       # 交互式向导
# 或全 flag 免交互（适合脚本/AI 代跑）：
miro init --archetype engineer --name kuro --owner alex --tz Asia/Shanghai
```

- `--archetype` 选人格原型：`companion`（情感陪伴）/ `engineer`（技术同行）/ `steward`（运营管家）/ `mentor`（导师式），或自定义关键词。
- init 会自动生成 workspace：`SOUL.md`（人格与准则）、`IDENTITY.md`、`USER.md`、`memory/`（记忆体系）、`TOOLS.md` 等，并安装一个 setup-guide 技能。
- 之后直接运行 `miro` 启动的就是「带身份的 Claude Code」——它记得你是谁、项目在哪儿、上次干到哪。

首次对话时，AI 会基于你的聊天自动充实人格（Day-0 自我丰富），不需要手写任何配置。

## 3. 日常使用：记忆怎么长出来

- 它说「记住了」= 真的写进了文件：当日记忆在 `memory/YYYY-MM-DD.md`，长期主题在 `memory/topics/*.md`，索引在 `MEMORY.md`。
- 纠正它的行为 → 会沉淀成 `memory/topics/feedback_*.md`，下次启动自动注入（Feedback 区）。
- 查历史：让它搜 `memory/`，或用 FTS5 全文检索 `miro db search-fts "关键词"`。
- 改了人格/记忆文件后可跑 `miro lint` 校验 frontmatter。

## 4. 派发第一个跨 CLI 任务（5 分钟）

前置：soul server 在跑（终端 A：`miro server --token <你的token>`），且至少一个其他编码 CLI 已登录。

在 Claude Code 会话里（或终端）：

```bash
miro spawn --bare --backend grok --model grok-4.6 \
  --project /absolute/path/to/your/repo \
  "审查 src/ 下的并发逻辑，列出最危险的 3 个隐患" --wait
```

逐项说明：

| 参数 | 含义 |
|---|---|
| `--bare` | **必须带**。真外包给外部 CLI；不带会退回 Claude 换皮，不会真正派发 |
| `--backend` | 目标 worker：`codex` / `grok` / `kimi` / `agy` / `opencode`（严格白名单，写错会明确报错） |
| `--model` | **必填**，各 backend 的原生模型 ID（见下表） |
| `--project` | worker 的工作目录（绝对路径） |
| `--wait` | 等待完成并把结果带回当前会话 |

**模型 ID 速查**（传错的表现是 `session failed: backend error`，先怀疑模型名）：

| backend | 可用 model 示例 |
|---|---|
| codex | `gpt-6-astra` |
| grok | `grok-4.6` |
| kimi | `kimi-code/k3` |
| agy | `gemini-3.8-flash-low`、`gemini-3.1-pro-low` |
| opencode | `opencode-go/glm-5.3-flash` 等 provider/model 全名 |

## 5. 什么活派给谁（实践建议）

- **重活/实现**：主力 backend；两个 backend 并行实现同一需求再对比，也是常见打法。
- **独立审查**：派给一个与实现者不同的 backend 做对抗审查，只挑毛病不改代码。
- **批量机械活**：用便宜的快模型，别烧旗舰额度。
- 调度策略怎么进化：每次纠正 worker 选择都会沉淀进 feedback，下次推荐更准（池化记账与自动降级在 Roadmap）。

## 6. 常见问题

**Q：`spawn` 没有真正派给外部 CLI？**
A：检查是否带了 `--bare` 且 soul server 在跑；server 日志在 `/tmp/soul-server.log`。

**Q：Windows 能用吗？**
A：社区提供 Linux 一键部署脚本（bootstrap.md 喂给 Claude Code 全自动装）；Windows 建议 WSL2 + 同样流程。

**Q：`go build` 报缺 `maps`/`slices` 包？**
A：Go 版本 < 1.21，升级到 1.25+（`brew install go` 或官网安装包）。

**Q：多 worker 的额度/花费在哪看？**
A：统一记账在 Roadmap；当前可从各 CLI 自己的后台查看，opencode 派发结果里自带 cost 字段。

---

*更多背景与设计思想见仓库 README 与《Soul 产品设计文档》。遇到问题欢迎进内测群（口令「跨模型」）。*
