# conformance — 契约符合性自检工具

对任意 ww3 渠道插件 / 拦截器插件做**契约符合性检查**，输出可直接写进插件注册表的摘要。

> **它不是准入门禁。** 本仓只交付工具；每个插件仓自己决定要不要在 workflow 里跑它。
> 注册表只有 `repo` / `version` / `file` / `sha256` 是必要信息，`conformance` / `capabilities`
> 全部可选——有就多显示一点，没有也照常安装（详见 [交付边界](#交付边界)）。

契约权威在 **[`ww3-protocol`](https://github.com/wild-work3/ww3-protocol)**（文本 + JSON Schema + 样例）。
本工具**刻意不复用**该仓的 Go 代码：按文本契约独立实现一份最小 framing，
这样「工具」与「实现」互为校验，实现漂移能被抓出来。

---

## 安装

```bash
# 有 Go 环境（推荐：零安装成本）
go install github.com/wild-work3/ww3-conformance/cmd/ww3-conformance@latest

# 或直接下载 release 二进制（Python / Node 插件作者不需要 Go）
#   https://github.com/wild-work3/ww3-conformance/releases/latest/download/ww3-conformance-<os>-<arch>[.exe]
#   os ∈ windows|linux|darwin，arch ∈ amd64|arm64
# 例（Windows x64）：
#   curl -fsSL -o ww3-conformance.exe \
#     https://github.com/wild-work3/ww3-conformance/releases/latest/download/ww3-conformance-windows-amd64.exe
```

## 三种用法

工具核心**零渠道知识**：渠道事实（mock 路由、夹具模型、能力缺口）全部来自描述文件。
删掉本仓的 `presets/` 目录，单插件模式照样完整可用。

### A 层：零配置（零网络）

只给可执行文件，检查契约骨架 + `capabilities.list` 与 manifest 的一致性：

```bash
ww3-conformance --exe /path/to/myext-ext.exe
```

> **插件产物名不必是 `<id>-ext`**（用户 m00634/m00642）：`--exe` 直接给路径，叫什么都能跑。
> B 层描述文件里也可以用 `"exe"` 字段声明产物名（相对描述文件目录，缺扩展名自动补 `.exe`，
> 所以同一份 `conformance.json` 能同时适配 `chatglm-cli`（Linux）与 `chatglm-cli.exe`（Windows））。

**不提供描述文件时，控制面与会话面自动 SKIP**（写 `SKIP` 而不是跑到真实上游）——
没有 mock 基址时那些调用会打到插件默认端点，等于用真实账号做探测，本工具绝不做这件事。

### B 层：自带 `conformance.json`（推荐）

作者声明 mock 路由、夹具模型、能力缺口，全部打本地 mock，零真实请求：

```bash
ww3-conformance --exe ./dist/myext-ext.exe --descriptor ./conformance.json
```

最小示例见 [`examples/openai-compat.json`](examples/openai-compat.json)（渠道插件）与
[`examples/loopguard.json`](examples/loopguard.json)（拦截器插件）。

### C 层：内置预设（本仓自带 19 个渠道的既有事实）

```bash
ww3-conformance --dist ../dist                    # 全部预设
ww3-conformance --dist ../dist --only glm         # 单个
ww3-conformance --list                            # 列出可跑的插件
```

`presets/*.json` 是 ww3 工作区内 19 个渠道的测试夹具事实，**格式与作者自写的
`conformance.json` 完全一致**。它**不是本工具的必需品**，也不构成对第三方的准入要求。

### 非单 exe 插件（Python / Node）

契约只要求「能被 spawn 起来跑 `call`/`stdio`/`manifest`」，从未限制实现语言。
`--` 之前是本工具的参数，之后**全部透传给被测进程**：

```bash
ww3-conformance --exe /path/to/python -- /path/to/myext/main.py
# 实际启动：/path/to/python /path/to/myext/main.py call --data <夹具目录>
```

---

## UI 插件：serve 契约断言 + 手动反代联调（W3-61）

manifest 声明了 `ui`（主面板/设置标签）的插件会多验一层 **Web 界面**（零上游网络，
A 层即可跑）：serve 就绪帧、只监听回环（`--listen 0.0.0.0:0` 必须被拒绝）、就绪后
stdout 静默、`/healthz`、每个 slot 的 entry 可访问、**页面无根绝对路径资源引用**
（`src="/…"` 这类在 gateway 反代剥前缀后会 404，契约明说 core 无法侦测，只能在此测）、
终止后无残留。插件专属 HTTP API 可在 `conformance.json` 里自证（完整示例
`examples/ui.json`）：

```json
{ "schema": 1, "id": "myext",
  "ui": { "probes": [{"path": "/api/config", "want_status": 200, "body_contains": "enabled"}] } }
```

### 如何验证一个 UI 插件（推荐流程）

**第 1 步：自动断言（CI 可跑，零上游网络）**

```bash
ww3-conformance --exe dist/myext-ext.exe                     # A 层：UI 层必跑（无需 conformance.json）
ww3-conformance --exe dist/myext-ext.exe --descriptor conformance.json   # B 层：额外验 ui.probes 自证 API
```

拿到报告后逐条核对 `Web 界面` 层：

| 断言 | 不过时怎么修 |
|---|---|
| `ui.declared` WARN | manifest 加 `"ui":{"main":[…],"settings":[…]}`，或删掉 serve 命令（声明与实现二选一） |
| `ui.serve.loopback.reject` | serve 里硬拒非 127.0.0.1 的 `--listen`（非 0 退出），gateway 也这么传参 |
| `ui.serve.quiet` | 就绪帧后**日志一律走 stderr/文件**，stdout 只许那一帧 serve-ready |
| `ui.serve.entry.*` | slot 的 `entry` 路径相对插件目录且 serve 能按 `/<entry>` 返回 200 text/html |
| `ui.serve.selfcontained.*` | 页面里 `src="/…"`、`href="/…"`、`url(/…)` 改成相对路径（见第 2 步） |
| `ui.serve.exit` | SIGTERM/退出信号后进程要退干净；Windows 下至少做到 Ctrl+C 能回收 |

**第 2 步：手动联调（非自动化，看真实页面）**

```bash
ww3-conformance --exe dist/myext-ext.exe --ui-proxy 127.0.0.1:18788 [--data <真实数据目录>]
```

浏览器打开打印出的入口（`/` 列出全部 slot 链接，形状与集成 gateway 后一致：
`/ext/<id>/<entry>`）。这个模式刻意复刻 gateway 的 `/ext` 分支，所以：

- **根绝对路径资源会 404、命中时 stderr 打 WARN** —— 「直连能显示、集成必错」的缺陷
  在集成前就能看见；点一遍页面把每个资源都触发到（动态 fetch 静态扫描测不到）。
- 页面里 `fetch('/core/…')`、`/gw/…`、`/v1/…` 会 404 —— 这些前缀归宿主（gateway）接管，
  插件页面应只调自己的 API（`/api/…`）或经 postMessage 与宿主协作。
- 要验登录态/凭据相关页面，用 `--data` 指向真实数据目录（缺省是隔离的临时夹具，`--keep` 可保留）。

**第 3 步：集成验收（归宿主，不归本工具）**

`--ui-proxy` 通过后再跑完整栈（gateway + core）验证宿主壳行为（标签排序、iframe、主题广播）——
那是 gateway 的契约，由主仓 `temp/test_ui_shell.py` 覆盖。

### `--ui-proxy` 生命周期与边界

- 长驻运行，Ctrl+C 退出并回收插件 serve 进程（不产报告、不写摘要，与准入判定解耦，**不进 CI**）。
- 与 `--dist`、`--json-summary` 等报告开关互斥（exit 2）；临时数据目录默认退出即清理。
- 无管理鉴权（本机回环、仅供开发调试）；反向 WARN「实现了 serve 却没声明 ui」在自动断言里。

---

## 四层可测深度与三态结论

| 层 | 触发条件 | 覆盖 |
|---|---|---|
| A | 只给 `--exe` | 契约骨架（manifest / 子命令 / stdout 纯 JSONL / 生命周期 / 能力一致性） |
| B | A + `--descriptor` | 控制面（按声明的能力逐条验）+ 会话面 + 多轮历史下发 + usage 口径 |
| C | `--dist`（本仓预设） | 19 个渠道全量回归 |
| L | `--mode live --data <凭据目录>` | 真实上游单轮抽查（人工，不进 CI） |

每条断言四态：**PASS / FAIL / WARN / SKIP**。

- `SKIP` **只允许**用于契约允许不实现的情形（能力未声明 / `conformance.json` 的 `gaps` 已声明缺口 / 无 mock 基址时跳过控制面会话面）。
- **无凭据、上游对抗规则变更、真实上游不可达一律 FAIL**（不是 SKIP）。
- `WARN` 表示「契约允许，但作者应知情」——最常见的是本工具能力缺口：`single_turn`（上游只接受单条 user 提问，无法验证多轮历史累积）、`no_token_usage`（上游不返回 token，只能验「不臆造」）。

## 输出：`--json-summary` 直接贴进注册表

```bash
ww3-conformance --exe ./dist/myext-ext.exe --descriptor ./conformance.json \
    --json-summary summary.json --print-snippet
```

产物形状（`schema:1`）：

```json
{
  "schema": 1,
  "id": "myext",
  "version": "1.0.0",
  "kind": "channel",
  "mode": "offline",
  "verdict": "PASS",
  "counts": { "pass": 47, "warn": 0, "fail": 0, "skip": 0 },
  "conformance": { "pass": 47, "warn": 0, "fail": 0, "mode": "offline", "at": "2026-11-08" },
  "capabilities": {
    "control": ["models.list", "accounts.list", "auth.methods"],
    "conversation": ["chat.generate"]
  },
  "ability": { "chat": true, "models": true, "accounts": true, "credits": true },
  "note": "…",
  "failures": []
}
```

- `conformance` 子对象就是注册表的可选字段，**原样粘贴**即可（`--print-snippet` 直接渲染成可粘的 JSON）。
- `capabilities` 取自 **`capabilities.list` 实测返回**，不是 manifest 声明——能抓到「manifest 声明了但未注册」这类不一致。
- `ability` 是**三态**：`true`（测过且通过）/ `false`（测过且失败）/ **键不出现**（未测，如 A 层跳过了控制面）。
  未测不等于不支持——把「没测」展示成「不支持」比不显示更误导。
- 摘要**不含凭据与端点**。

## 接进自己的 CI

[`templates/publish.yml`](templates/publish.yml) 是一份可拷贝的 workflow 范例（A 层零配置 + 有
`conformance.json` 时自动升 B 层，不碰注册表）。**它只是范例，不是门禁**：加不加由你决定，不加同样能发布
（注册表只校格式）。

想要**完整闭环**（构建 → 自检 → 自动生成注册表条目 → 提 PR）的现成范本，看
[`ww3-workbuddy-ext`](https://github.com/wild-work3/ww3-workbuddy-ext) 的
`.github/workflows/release.yml` + `scripts/make_registry_entry.py` + `conformance.json`。
靠本仓 release 预编译的二进制即可在插件仓里跑，**不需要装 Go**：

```bash
curl -fsSL --retry 3 \
  https://github.com/wild-work3/ww3-conformance/releases/latest/download/ww3-conformance-windows-amd64.exe \
  -o ww3-conformance.exe
./ww3-conformance.exe --exe dist/<id>-ext.exe --descriptor conformance.json \
  --json-summary conformance-summary.json
```

发布二进制由 [`.github/workflows/release.yml`](.github/workflows/release.yml) 打 tag `v*` 时产出，
6 平台资产名固定为 `ww3-conformance-<os>-<arch>[.exe]`（拼 `releases/latest/download/<名字>` 即得，不依赖 tag 命名）。

---

## 交付边界

本仓**只交付工具**，明确不做这些事：

- 不负责跑任何插件的验证，不维护「谁验过」的账；
- 不代插件仓提注册表 PR；
- 不把 `conformance` 字段做成注册表必填（注册表只有 `repo`/`version`/`file`/`sha256` 必要）。

插件提供这两个字段，插件面板就多显示一点；不提供，照常展示、照常安装。

## 开发

```bash
go build ./... && go vet ./... && go test ./...
go run ./cmd/ww3-conformance --dist ../dist          # 全量回归（需要 ww3 工作区的 dist/）
```

设计依据与完整验收口径见 [`PLAN.md`](PLAN.md)。
