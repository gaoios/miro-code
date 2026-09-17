# 5. 派发第一个跨 CLI 任务（5 分钟）

**前置**：soul server 在跑（终端 A：`miro server --token <你的token>`），且至少一个其他编码 CLI 已登录。

```bash
miro spawn --bare --backend grok --model grok-4.6 \
  --project /absolute/path/to/your/repo \
  "审查 src/ 下的并发逻辑，列出最危险的 3 个隐患" --wait
```

## 参数逐项说明

| 参数 | 含义 |
|---|---|
| `--bare` | **必须带**。真外包给外部 CLI；不带会退回 Claude 换皮，不会真正派发 |
| `--backend` | 目标 worker：`codex` / `grok` / `kimi` / `agy` / `opencode`（严格白名单，写错会明确报错） |
| `--model` | **必填**，各 backend 的原生模型 ID（见下表） |
| `--project` | worker 的工作目录（**绝对路径**） |
| `--wait` | 等待完成并把结果带回当前会话 |

## 模型 ID 速查

⚠️ 传错模型名的表现是 `session failed: backend error` 而不是「模型不存在」——遇到这个报错先怀疑模型名。

| backend | 可用 model 示例 |
|---|---|
| codex | `gpt-6-astra` |
| grok | `grok-4.6` |
| kimi | `kimi-code/k3` |
| agy | `gemini-3.8-flash-low`、`gemini-3.1-pro-low` |
| opencode | `opencode-go/glm-5.3-flash` 等 provider/model 全名 |

> 上一章：[日常使用与记忆体系](04-memory.md) · 下一章：[什么活派给谁](06-routing.md)
