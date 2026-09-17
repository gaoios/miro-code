# 7. 常见问题 FAQ

**Q：`spawn` 没有真正派给外部 CLI？**
A：检查是否带了 `--bare` 且 soul server 在跑；server 日志在 `/tmp/soul-server.log`。

**Q：Windows 能用吗？**
A：建议 WSL2 + 同 Linux 流程；社区提供 Linux 一键部署（把仓库里的 `bootstrap.md` 喂给 Claude Code 即可全自动安装）。

**Q：`go build` 报缺 `maps` / `slices` 包？**
A：Go 版本 < 1.21，升级到 1.25+（`brew install go` 或官网安装包）。若本机有多个 Go 且 GOROOT 指向旧版，`unset GOROOT` 后重试。

**Q：多 worker 的额度 / 花费在哪看？**
A：统一记账在 Roadmap；当前可从各 CLI 自己的后台查看，opencode 派发结果里自带 cost 字段。

**Q：我的 Key 安全吗？**
A：Key 全程留在本机，miro-code 不托管、不上传、不转售。

**Q：遇到别的问题？**
A：进内测群（口令「跨模型」），或到 [GitHub Issues](https://github.com/gaoios/miro-code/issues) 提问。

> 上一章：[什么活派给谁](06-routing.md) · [返回教程首页](../tutorial.zh.md)
