# 1. 是什么 / 准备什么

miro-code 做两件事：

1. **给 Claude Code 装上持久身份与记忆**——每次启动不再是白纸，它记得你是谁、项目在哪儿、上次干到哪、你纠正过它什么。
2. **跨 CLI 任务派发**——一条 `spawn --bare` 命令，把编码任务派给你本机已登录的其他编码 CLI（codex / grok / kimi / agy / opencode），结果回到当前会话。

Key 与凭据全部留在你本机，miro-code 不托管、不上传任何账号信息。

## 准备清单

| 依赖 | 说明 |
|---|---|
| Go 1.25+ | 编译用；`go version` 确认 |
| Claude Code | 已安装并登录（`claude --version` 能出版本号） |
| （可选）其他编码 CLI | codex / grok / kimi / agy / opencode 任意一个已登录，才能派发给它 |

> 上一章：[返回教程首页](../tutorial.zh.md) · 下一章：[安装](02-install.md)
