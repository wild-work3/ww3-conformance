# presets/ — ww3 工作区的内置预设（数据，不是代码）

每个 `<id>.json` 是**一个渠道的测试夹具事实**：mock 上游路由、夹具模型名、凭据夹具、
能力缺口声明。格式与作者自己写的 `conformance.json` **完全一致**（同一个 `suite.LoadFile` 解析器）。

## 它们不是必需品

- 工具核心**零渠道知识**：删掉整个 `presets/` 目录，`--exe` 单插件模式照样完整可用。
- 这些文件只为「ww3 工作区内 19 个渠道一次性全量回归」服务（`--dist ../dist`）。
- 第三方插件**永远不需要改本仓**：写自己的 `conformance.json` 即可。

## 怎么来的

改造前这些渠道事实硬编码在 `internal/suite/descriptors/channels.go`（751 行 Go 代码）里。
为了让工具核心零渠道知识，它们被**一次性导出**成本目录下的 JSON 数据，
随后那 751 行 Go 代码与导出器一并删除。

因此这些文件是**渠道行为的历史镜像**：改渠道实现后它们不会自动跟着变，
需要人工核对更新。第三方插件不要依赖它们，那些是 ww3 自己的渠道。

## 与 `--dist` 的关系

```bash
ww3-conformance --dist ../dist              # 跑全部预设
ww3-conformance --dist ../dist --only glm   # 跑单个
ww3-conformance --list                      # 列出可跑的插件
```

exe 路径按约定解析（`<id>-ext` → `<id>-hook` → 裸 `<id>`，见 `internal/suite/descriptors/exe.go`）。
