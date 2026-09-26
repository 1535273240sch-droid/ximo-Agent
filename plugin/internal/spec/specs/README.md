# 内置规格正文

本目录是**内置适配器规格的唯一正文**，通过 `//go:embed specs/*.json` 编译进
`ximo-plugin` 二进制（`internal/spec/load.go`）。之所以放在 `internal/spec/` 下：
`//go:embed` 不能跨目录引用，写在 `plugin/specs/` 无法被编译进二进制。

- 加/改规格：直接编辑本目录的 `*.json`，**文件名必须等于 `id`**。
- 用户自定义规格：`~/.ximo-plugin/specs/*.json`，同名 `id` 会整体覆盖这里的规格。
- 完整字段说明与新增步骤见 [`plugin/specs/README.md`](../../../specs/README.md)。

本目录内的 `*.md` 不会被加载（加载器只认 `*.json`）。
