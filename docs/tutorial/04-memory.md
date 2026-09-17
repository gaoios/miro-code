# 4. 日常使用与记忆体系

## 记忆怎么长出来

- 它说「记住了」= 真的写进了文件：当日记忆在 `memory/YYYY-MM-DD.md`，长期主题在 `memory/topics/*.md`，索引在 `MEMORY.md`。
- **纠正它的行为 → 沉淀成规则**：你的纠正会写进 `memory/topics/feedback_*.md`，下次启动自动注入（Feedback 区），同样的错不再犯。
- 会话结束不丢上下文：新会话启动时自动装载今天/昨天的日记与长期索引。

## 检索与校验

- 查历史：直接让它搜 `memory/`，或用全文检索 `miro db search-fts "关键词"`。
- 改了人格/记忆文件后跑 `miro lint` 校验 frontmatter。

> 上一章：[初始化 workspace](03-init.md) · 下一章：[派发第一个跨 CLI 任务](05-dispatch.md)
