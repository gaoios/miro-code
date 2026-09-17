# 3. 初始化 workspace（3 分钟）

```bash
miro init                       # 交互式向导
# 或全 flag 免交互（适合脚本/AI 代跑）：
miro init --archetype engineer --name kuro --owner alex --tz Asia/Shanghai
```

## 人格原型怎么选

| Archetype | 风格 |
|-----------|------|
| `companion` | 情感陪伴型——记得小事、察觉情绪 |
| `engineer` | 技术同行——代码优先、解释在后、冷幽默 |
| `steward` | 运营管家——主动、有条理、可靠 |
| `mentor` | 导师式——苏格拉底式提问、分层讲解 |
| *(custom)* | 用关键词自定义 |

init 会自动生成 workspace：`SOUL.md`（人格与准则）、`IDENTITY.md`、`USER.md`、`memory/`（记忆体系）、`TOOLS.md` 等，并安装一个 setup-guide 技能。

之后直接运行 `miro` 启动的就是「带身份的 Claude Code」。首次对话时，AI 会基于你的聊天自动充实人格（Day-0 自我丰富），不需要手写任何配置。

> 上一章：[安装](02-install.md) · 下一章：[日常使用与记忆体系](04-memory.md)
