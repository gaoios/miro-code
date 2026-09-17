# 2. 安装（3 分钟）

```bash
git clone https://github.com/gaoios/miro-code.git
cd miro-code
go build -o miro .
mv miro ~/go/bin/        # 或放到任意 PATH 目录
miro version             # 应输出 miro x.x.x
```

## 验证安装

```bash
miro --version   # 版本号
miro --help      # 命令总览
```

> Windows / 未装 Go？见 [FAQ](07-faq.md)。
>
> 上一章：[是什么 / 准备什么](01-introduction.md) · 下一章：[初始化 workspace](03-init.md)
