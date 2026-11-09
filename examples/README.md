# examples/ — 描述文件范例

## `openai-compat.json`

最小可用的 OpenAI 兼容渠道描述文件：两个路由（模型目录 + 对话流），夹具模型 `mock-flash`。
适合「上游就是标准 OpenAI SSE」的渠道，拷进自己仓改 `path` 与模型名即可。

## `loopguard.json`

拦截器插件（`kind=hook`）的描述文件，演示 `hook.terminate` 段：
推一串重复行触发死循环检测，断言返回的错误 `kind == "reasoning_loop"`。

注意：hook 的描述文件**不放 `presets/`**——`presets/` 是渠道预设，
`--dist` 全量回归会把它当渠道跑（缺 mock 路由、会话面跳过，语义错位）。
拦截器请用 `--exe ... --descriptor examples/loopguard.json` 单独跑。

## 怎么用

```bash
# 拷到你自己仓根目录，改内容
cp examples/openai-compat.json ../my-ext/conformance.json

# 跑（与 exe 同目录时自动发现，不必传 --descriptor）
ww3-conformance --exe ../my-ext/dist/my-ext.exe

# 或显式指定
ww3-conformance --exe ../my-ext/dist/my-ext.exe --descriptor ../my-ext/conformance.json
```
