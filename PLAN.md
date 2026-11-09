# conformance 改造计划：从「工作区内全渠道套件」到「独立插件自检 CLI」

> 状态：**已实施（W3-53，阶段 1–4 全部落地）**。本文保留作为设计依据与验收口径。
>
> 实施结果（可复核）：
> - 阶段 1：`--exe` / `--` 透传 / `--descriptor` / `--preset` 已落地；`--dist ../dist` 全量 **PASS=876 / FAIL=0 / WARN=22 / SKIP=1**，与改造前基线**逐字相同**。
> - 阶段 2：`conformance.json` 解析 + `--json-summary` / `--print-snippet` 已落地；`ability` 矩阵为**三态**（true/false/键不出现=未测）。
> - 阶段 3：hook 层已落地（`hook.declared` / `hook.stream` / `hook.passthrough` / `hook.end` / `hook.no-error-event` / `hook.no-cache` / `hook.terminate`），loopguard 首次有准入用例（23 条）。
> - 阶段 4：`templates/publish.yml` + `examples/` 已交付。
> - **额外完成的去渠道化（W3-53 用户定案）**：19 个渠道预设从 Go 代码降级为 `presets/*.json` 数据，工具核心**零渠道知识**；`descriptors/channels.go`（751 行）与 `openai.go`（136 行）已删除。
> - module path 改为 `github.com/wild-work3/ww3-conformance`，`go` 指令降到 `1.21`，零第三方依赖。
>
> 上位契约：`ww3-protocol` 仓（`docs/*.md`）、`docs/PLUGIN_REGISTRY.md`（W3-48/W3-50）、`AGENTS.md` §2-AG（W3-36）。

---

## 0. 一句话目标

让 `ww3-conformance` 成为一个**可被任意渠道/hook 插件仓库直接调用的独立 CLI 工具**：
输入一个插件 exe（可选加一份描述文件），输出 **契约符合性判定 + 能力矩阵 + 可直接粘进插件注册表的结果**，
使插件作者能把它作为自己发布流程（`build.sh --zip` → 打 release → 提注册表 PR）的一步。

### 0.1 交付边界（用户 m02729 定案，先读这条）

> 「我们只是通过独立工具仓库的方式交付一个兼容性测试工具，并不负责具体每个插件的验证，
> 每个插件应该自己在 workflow 中增加验证步骤，当然我们可以在示例性仓库内给出范例。
> 但是这个验证步骤，以及对应的注册表字段不做强制要求。如果插件提供了这些数据，
> 在 ww3 插件管理界面上，就能呈现更多信息给用户。」

这条把「谁做什么」钉死，改造时不得越界：

| 角色 | 负责 | 不负责 |
|---|---|---|
| **本仓（`conformance/`）** | 交付**工具**：`ww3-conformance` CLI + 描述文件 schema + 摘要格式 + 一份 workflow 模板；保证工具对任意插件可跑、输出稳定可解析 | **不负责跑任何插件的验证**，不维护「谁验过、验出什么」的账；不代任何插件仓提交注册表 PR |
| **每个插件仓** | 自己决定要不要在 workflow 里加验证步骤（用本仓工具）；自己把摘要写进注册表 PR | 不强制：不加也能发布（注册表只校 `repo`/`version`/`file`/`sha256`） |
| **`ext-registry`**（远程 `ww3-ext-registry`） | 只校格式（JSON/命名/`id` 唯一/拒二进制） | 不校 `conformance`/`capabilities` 真伪，不做门禁 |
| **ww3 面板/商店** | 有这两个字段就**多显示一点**（如「准入 47/47」「能力矩阵」徽章）；没有就照常展示基础信息 | 不得因缺字段而禁用安装/隐藏条目 |

**因此本工具不是准入门禁**：用户 m02550「只有仓库、版本、文件名、sha 等是必要信息，其它均为可选」+ 用户 m02729 的边界。
工具的输出是**作者自证 + 读者参考**，注册表侧只校格式不校真伪（`ext-registry/scripts/validate.sh` 现状即如此）。

**「示例性仓库给范例」的落点**：本仓提供 `templates/publish.yml`（可直接拷进插件仓的 workflow 片段）+
`examples/conformance.json`（描述文件范例），但**不替任何插件仓启用它**，也不在主仓 `build.sh`/回归里跑它。

---

## 1. 现状（实测事实，改造的出发点）

### 1.1 只能跑「工作区里已登记的那 19 个渠道」

- `conformance/internal/suite/descriptors/channels.go:689` 的 `func All(distDir, liveAuthDir string) []suite.Descriptor`
  **硬编码 19 个渠道**：每个 `Descriptor` 写死 exe 路径、夹具模型名、mock 路由表、`ExtraConfig`、`Auth` 夹具、
  `SingleTurn`/`NoTokenUsage`/`Session`/`SessionCID`/`AuthPrefixes`/`LiveModel`。这些是**渠道事实**，不可能由工具推导。
- CLI（`conformance/cmd/ww3-conformance/main.go:44-59`）只有
  `--dist/--only/--skip/--mode/--out/--work/--keep/-v/--list/--plan/--live-auth/--data/--auth/--model/--live-turns/--timeout`，
  **没有 `--exe`**。传一个未登记的插件 ID 只能得到：
  ```
  没有可跑的渠道（检查 --only/--skip 与 --dist）   退出码 2
  ```
- 结论：第三方插件**无法**用它自检；甚至**hook 插件完全没有准入用例**（实测 `--only loopguard` → 退出码 2）。

### 1.2 报告形状不面向注册表

- `internal/report` 只有整份 `Bundle`（`WriteJSON`/`WriteMarkdown`），字段是 `{started,ended,mode,env,totals,verdict,plugins[]}`，
  每个 plugin 是完整 `check.PluginReport`（含全部 `Checks[]`）。
- 注册表需要的是 `conformance: {pass,warn,fail,mode,at}`（`docs/PLUGIN_REGISTRY.md:102`），
  现在由 ww3 主仓的 `temp/gen_registry.py` 的 `load_conformance()` 从本机手跑的报告里**抠出来**——
  也就是说注册表里的这个字段是「主仓发版时本机手跑一次」的产物，不是插件仓 CI 的产物。
  这与 §0.1 的交付边界相违（**主仓不代插件跑验证**），改造后应改为「读插件仓自己提交的摘要，读不到就不写该字段」。

### 1.3 已具备、可原样复用的能力（改造不必重写）

| 能力 | 位置 | 说明 |
|---|---|---|
| 最小 JSONL framing（自实现，不复制 `main/protocol`） | `internal/wire` | `Spawn`/`Call`/`Push`/`RunOnce`/`InvokeFrames`，`Call.OnPush` 已支持双向流（`hook.stream` 用） |
| 可编程 mock 上游 | `internal/mock` | 8 种响应形态 + `Range`/延迟/`NoUsage`，未命中 404 并记录请求 |
| 插件夹具 | `internal/harness` | `NewEnv(root,id,exe)` 建 `<root>/data/{auths,logs,stat,auths/flows}`、`WriteAuth`/`WriteConfig`/`CallWithConfig`/`RunCmd` |
| 四态判定 | `internal/check` | `PASS/FAIL/WARN/SKIP` + 七层 `Level` + `PluginReport.Verdict()`/`ByLevel()`/`Failures()` |
| 断言集合 | `internal/suite/{contract,control,chat,live}.go` | 契约骨架 / 控制面 / 会话面+多轮+用量 / 真实上游 |
| 稳定断言 ID | 同上 | 如 `manifest.load`/`manifest.execution`/`call.unknown`/`chat.upstream`/`usage.flags`/`live.multi`，可跨版本对比 |

改造前的离线全量基线（19 渠道，报告已留在 ww3 工作区未随本仓发布）：
**PASS=876 / FAIL=0 / WARN=22 / SKIP=1**，单渠道 44~55 条断言（kilo 46、glm 55+1W、chatgpt 52+1W+1S）。

---

## 2. 目标形态：三层「可测深度」，按作者投入递增

同一个 exe，能测多深取决于作者提供了多少**渠道事实**。工具必须让「零配置也能跑出有意义结果」成立，
否则没人会在自己仓里接它。

| 层 | 作者要做的事 | 覆盖的断言层 | 是否有网络请求 | 典型用途 |
|---|---|---|---|---|
| **A 零配置** | 什么都不做，只给 `--exe` | 契约骨架（`manifest.*`/`call.*`/`stdout.pure`/`lifecycle.*`）+ `capabilities.list` 一致性 | **零**（只 spawn 本机进程） | 任何插件仓 CI 的默认一步 |
| **B 自带描述文件** | 仓里放一份 `conformance.json`（声明 mock 路由、夹具模型、`ExtraConfig`、凭据夹具） | + 控制面（按声明能力逐条）+ 会话面 + 多轮 + 用量口径 | 零（全部打本地 mock） | 正式发布前的准入自检 |
| **C 工作区内置** | 主仓 `descriptors/` 里的 19 份预设 | 同 B（预设里含渠道事实） | 零 | `--dist ../dist` 全量回归（**现状不变**） |
| **L live** | 提供真实凭据 | + `live.*`（真实上游，flash 类模型，默认 1 轮） | 有 | 人工抽查，**不进 CI** |

关键设计约束（沿用现有红线）：

- **A 层绝不允许发出真实上游请求**。现状有个陷阱：`hasMock=false` 时 `env.Config` 里没有 `base_url`，
  而 `models.list` 这类控制面调用会走插件默认端点（真上游）。A 层因此**只跑契约骨架 + `capabilities.list`**，
  控制面整体跳过并在报告里写明「未提供 mock 描述文件，控制面未测」——不许静默去碰真上游（AGENTS §6）。
- **B 层的 mock 路由由作者提供，工具必须反向自证**：`chat.upstream`（"请求到达 mock 上游"）断言已经存在，
  B 层要把它升级为**强制项**——mock 一次请求都没收到 = FAIL，否则「路由写错了但插件压根没发请求」也会 PASS。
- **`mode: offline` 的语义不变**：只证明「插件 ↔ 契约 ↔ mock 上游」对得上，不证明对抗算法/风控/凭据有效性。
  报告与摘要里必须有这句原话（现有 `WriteMarkdown` 已写，摘要也要写进 `note`）。

---

## 3. 交付物设计

### 3.1 CLI 新增开关

| 开关 | 作用 |
|---|---|
| `--exe <path> [-- <插件参数>…]` | **单插件模式**：`--exe` 给**解释器或程序**；`--` 之后的参数原样透传给被测进程（见 §3.1.1）。与 `--dist` 互斥 |
| `--id <id>` | 显式指定插件 ID（缺省从 `manifest.id` 取；仅用于 manifest 拿不到时的兜底提示） |
| `--descriptor <file>` | 载入 B 层描述文件；缺省在同目录找 `conformance.json` |
| `--preset <id>` | 用 C 层内置预设（等价于现在 `--only <id>` 的单渠道形态） |
| `--json-summary <file>` | 输出 §3.3 的**注册表可用摘要**（默认 `<out>/conformance-summary.json`） |
| `--with-checks` | 摘要里附全部断言（体积大，排查用；默认只给计数与失败清单） |
| `--strict` | 把 WARN 也算失败（默认不算，与现状一致） |
| `--print-snippet` | 打印可直接贴进 `<id>.json` 的 `conformance`/`capabilities` 片段 |

`--dist` / `--only` / `--skip` / `--mode` / `--out` / `--live-*` 全部保持现状语义，**老用法零改动**。

#### 3.1.1 `--exe` 与 `--`：`--` 前是工具参数，`--` 后是插件参数（用户 m02793 / m02861）

> m02793：「conformance 的 cli 参数设计不合理，因为不能假定渠道插件也是单 exe，有可能是 `python.exe xxx.py`。
> 建议改成 `--exe <path> ... -- xxx.py`，也就是 `--exe` 后面可以跟解释器 exe 路径，`--` 后面的参数透传给 exe。」
> m02861：「`--` 前的是 conformance 的参数，后面是 exe 的参数，这个逻辑很简单啊。」

**不能假定「插件 = 单个 exe」**：契约只要求「能被 spawn 起来跑 `call`/`stdio`/`manifest` 子命令」，
`protocol/docs/PLUGIN_PROTOCOL.md` 从未限制实现语言。Python/Node 插件的真实命令行是
`python.exe main.py call --data <dir>`，解释器与脚本路径必须能表达出来。

**规则就一条**（不再有 `--exe-arg` 这类第二套写法）：

```
ww3-conformance [工具自己的开关…] --exe <程序路径> -- <给被测进程的参数…>
   ↑ 全部是 conformance 的参数            ↑ 全部透传给被测进程
```

`--` 之后的那串作为**前缀**交给被测进程，conformance 自己的子命令与 `--data` 追加在其后：

```
argv = [--exe 的值] ++ [-- 之后的参数…] ++ [子命令(call|stdio|manifest), "--data", <夹具目录>]
```

例：

```bash
# 单 exe 插件（不带 --，零改动）
ww3-conformance --exe ../dist/workbuddy-ext.exe

# Python 插件：解释器 + 脚本路径
ww3-conformance --exe /path/to/python -- /path/to/myext/main.py

# 实际启动：/path/to/python /path/to/myext/main.py call --data <夹具目录>
```

**实现要点**：

1. `suite.Descriptor` 加 `Args []string`（= `--` 之后的参数；不含子命令与 `--data`）。
   19 份内置预设全 `nil`，**行为逐字节不变**。
2. `harness.Env` 加 `Args`，`NewEnv(root,id,exe,args)`；`RunCmd`/`Call`/`Stdio` 三处拼命令收拢为一个
   `e.argv(sub string) []string`（现在 `harness.go:166/195/242` 各拼了一次）。
3. **`--dist` 预设模式不接受 `--` 参数**（多插件共用一份参数没有语义）→ 退出码 2。
4. `--exe` 不存在/不可执行 → **退出码 2**（用法错误），不是 1（判定失败）。
5. 摘要里 `exe` 仍是程序路径，加 `exe_args`（= `--` 之后的参数）；**有 `exe_args` 时省略 `exe_sha256`**
   （对解释器取摘要没有意义），并在 `note` 写明「被测进程由解释器启动，未记录二进制摘要」。
6. `internal/suite/contract.go:89` 的 `strings.HasSuffix(strings.ToLower(env.Exe), "glm2cli.exe")`
   这类**按 exe 文件名判定**的兼容分支，改造时改成「看 `env.Exe` 或 `env.Args` 是否含该名字」，
   否则 Python 化之后这条 WARN 降级会失效。

验收（加进阶段 1）：`--exe python.exe -- <一个最小 Python 插件>.py` 能跑通契约骨架；
`--exe nosuch.exe` 与「`--` 参数配 `--dist`」都退出码 2；`--dist ../dist` 全量计数与改造前**逐项相等**。

### 3.2 `conformance.json`（B 层描述文件）—— 契约化的 `suite.Descriptor`

作者写在**自己插件仓根目录**，工具读取后转成 `suite.Descriptor`。字段与 `suite.Descriptor` 一一对应，
只是把 Go 的 `Routes []mock.Route` 变成 JSON 数组。最小可用样例：

```json
{
  "schema": 1,
  "model": "mock-flash",
  "extra_config": { "api_key": "sk-mock-1234567890" },
  "auth": { "access_token": "mock-token" },
  "account": { "uid": "mock-1", "nickname": "mock" },
  "routes": [
    { "method": "GET",  "path": "/models",           "kind": "json",
      "body": "{\"data\":[{\"id\":\"mock-flash\"}]}" },
    { "method": "POST", "path": "/chat/completions", "kind": "sse_openai",
      "text": "你好，我是 mock 上游。", "usage": { "prompt_tokens": 100, "completion_tokens": 20 } }
  ],
  "single_turn": false,
  "no_token_usage": false,
  "session": false,
  "notes": ["本渠道上游只接受单条 user 提问（如实填写，工具会据此判 WARN 而非 FAIL）"]
}
```

硬约束（写进 schema，工具校验）：

- `schema` 必须为 `1`（前向兼容锚点，不认识的值 → 拒绝载入并报错，不静默降级）。
- `routes[].path` 是**正则**（同 `mock.Route.Path`），`kind` 取值必须是 `internal/mock` 已支持的枚举。
- **`notes`/`single_turn`/`no_token_usage`/`session` 必须诚实填写**：这几个字段的作用是让工具把「上游真实的能力缺口」
  判成 WARN 而不是 FAIL。填错会让报告撒谎（`protocol/docs/WEB_CLI_CONTRACT.md` §「gaps/quirks 必须诚实填写」同一条纪律）。
- 描述文件**不含凭据**：`auth`/`account` 只允许放 mock 夹具值；真实凭据只经 `--data`/`--auth` 传入（W3-07 零 token 落盘）。

### 3.3 注册表可用摘要（`--json-summary`）

```json
{
  "schema": 1,
  "id": "workbuddy",
  "version": "1.0.0",
  "kind": "channel",
  "exe_sha256": "…（可选，便于确认测的是哪份二进制；**有 `exe_args` 时不写**）",
  "exe_args": ["/path/to/myext/main.py"],
  "mode": "offline",
  "at": "2026-11-06T11:10:00+08:00",
  "verdict": "PASS",
  "counts": { "pass": 47, "warn": 0, "fail": 0, "skip": 0 },

  "conformance": { "pass": 47, "warn": 0, "fail": 0, "mode": "offline", "at": "2026-11-06" },

  "capabilities": {
    "control": ["models.list", "accounts.list", "auth.methods", "…"],
    "conversation": ["chat.generate"]
  },
  "ability": {
    "chat": true, "models": true, "accounts": true, "credits": true,
    "checkin": false, "resource_detail": true, "stats": true,
    "auth": "device_poll", "session": false, "images": false, "systemone": false
  },

  "note": "offline 模式：仅验证契约与归一化逻辑，未访问真实上游；上游对抗算法/风控/凭据有效性未验证。",
  "failures": [ { "level": "控制面", "id": "credits.get", "detail": "…" } ]
}
```

- **`conformance` 子对象就是注册表 `<id>.json` 里那个字段**，`--print-snippet` 会把它（连同 `capabilities`）渲染成可直接粘贴的 JSON。
- `capabilities` 取自 **`capabilities.list` 的实测返回**，不是 manifest 的声明 —— 这是对注册表现有
  `capabilities`（`docs/PLUGIN_REGISTRY.md:103`，当前靠手写/生成器抄 manifest）的实质改进：
  它能抓到「manifest 声明了 `accounts.checkin` 但 `capabilities.list` 没注册」这类不一致（`lifecycle.caps` 断言已在校验，摘要把它透出）。
- `ability` 是给面板/商店展示用的**派生布尔**，全部由断言结果推导，不新增探测请求。
- **摘要绝不包含任何凭据/端点**（`exe_sha256` 是二进制摘要，允许；`exe_args` 只记 `--` 之后那些启动插件必需的参数
  如解释器脚本路径，**不记凭据**）。

---

## 4. 分阶段落地步骤

### 阶段 1 · 单插件模式（A 层）—— 最小可用，先让「任意 exe 能跑起来」

1. 新增 `internal/suite/descriptor.go`：
   - `func Auto(exe string, args []string) (Descriptor, error)`：spawn `<exe> <args…> manifest` 取 id/name/version/kind/capabilities，
     构造**无 Routes** 的 `Descriptor`（`Args` 原样带上，见 §3.1.1）。
   - `func LoadFile(path string) (Descriptor, error)`：解析 §3.2 的 `conformance.json`，`schema` 不认识即报错。
   - `func (d *Descriptor) Validate() error`：`Routes` 非空时 `Kind` 必须是 `channel`；`hook` 类不接受 `Routes`（走阶段 3）。
2. `internal/suite/suite.go` 的 `Run` 增加「无 mock 时的安全边界」：
   - `hasMock == false` 且 `d.Kind == "channel"` → **跳过 `control` 整层**，写一条 `SKIP`：
     `control.skipped`「未提供 conformance.json，控制面需要 mock 上游，已跳过（避免打到真实上游）」。
   - `hasMock == false` 且声明了 `chat.generate` → `chat.*` 整层 `SKIP`，理由同上。
   - 契约骨架、`lifecycle.caps` 一致性、`stdout.pure`、`call.unknown` 等**不依赖网络**的断言照跑。
3. `cmd/ww3-conformance/main.go` 增加 `--exe`/`--id`/`--descriptor`/`--preset` 与 `--` 之后透传给插件的参数，
   把 `descriptors.All` 的选取逻辑抽成 `pickDescriptors(...)`，与 `--dist` 路径共用下游流程。
   注意 Go 的 `flag` 包在遇到第一个非 flag 参数就停止解析，`--` 之前必须自己切一刀（`os.Args` 里定位 `--` 再分两段解析）。
4. 退出码语义补一条：**`--exe` 指向的文件不存在 → 退出码 2（用法错误），不是 1（判定失败）**。

验收：`go run ./cmd/ww3-conformance --exe ../dist/kilo-ext.exe` 能跑完并给出契约骨架结论；
`--exe ../dist/nosuch.exe` 退出码 2 且文案可读；`--dist ../dist` 全量结果与改造前**逐项一致**。

### 阶段 2 · 描述文件 + 摘要（B 层 + 注册表对接）

1. `internal/report/summary.go` 新增 `type Summary struct{…}` + `func (b *Bundle) Summary() *Summary` + `WriteSummary(path)`；
   复用 `PluginReport.Counts()/Verdict()/Failures()`，不重复实现判定。
2. `--json-summary` 默认落在 `--out` 目录（`conformance-summary.json`）；`--print-snippet` 打到 stdout。
3. `internal/suite/descriptor.go` 的 `LoadFile` 完整实现 §3.2 schema，附 `internal/suite/descriptor_test.go`：
   合法样例、`schema:2` 拒绝、未知 `kind` 拒绝、`routes[].path` 非法正则拒绝、`hook` 带 routes 拒绝。
4. `ability` 派生逻辑放 `internal/report/summary.go`（纯函数，输入 `PluginReport`，输出 map），单测覆盖：
   `credits.get` FAIL → `ability.credits=false`；`auth.methods` 六范式之一 → `ability.auth` 取该 Type。

验收：`--exe dist/workbuddy-ext.exe --descriptor workbuddy-ext/conformance.json --json-summary x.json`
产出的 `conformance` 子对象能被 `ext-registry/scripts/validate.sh` 接受的形状；
把它手工贴进 `workbuddy.json` 后 `registry.ParseFile` 不报 warn。

### 阶段 3 · hook 插件支持（当前完全空白）

现状：`descriptors.All` 里没有 hook，`--only loopguard` 退出码 2。而 `protocol/docs/HOOK_PROTOCOL.md` 已冻结完整契约。

1. `internal/check/check.go` 增加 `LevelHook Level = "拦截器"`，并加入 `ByLevel` 的 order（放在 `LevelChat` 之后）。
2. 新增 `internal/suite/hook.go`，通用断言（不需要渠道事实）：
   | ID | 断言 | 判据 |
   |---|---|---|
   | `hook.stream` | `hook.stream` 可调用 | `capabilities.conversation` 含 `hook.stream`（否则 SKIP，契约允许） |
   | `hook.passthrough` | 事件原样透传 | push `text.delta`×3 → 收到同样 3 帧、`data` 逐字节相等（`HOOK_PROTOCOL §3.2`） |
   | `hook.end` | `upstream.end` 不转发 | push `upstream.end{ok:true}` → 下游**不得**收到该帧，`result` 为 `{ok:true}` |
   | `hook.no-error-event` | 不自行发 `error` 终止帧 | 全帧扫描无 `event=error`（`§2-Y` 红线：终止事件只能由 core 发） |
   | `hook.no-cache` | 不做整段缓存 | 首帧透传延迟 < 阈值（如 300ms），不随总帧数增长（`§3.2.1` 硬约束 1） |
3. 渠道/插件专属的**主动终止**用例放描述文件：`"hook": {"terminate": {"frames": [...], "want_kind": "reasoning_loop"}}`，
   命中后断言 `result.ok == false` 且 `error.kind == want_kind`、`scope == "model"`。loopguard 的 `guard` 命令已有离线场景，
   可以直接复用它的「复读同一长行 12 次」输入。

验收：`--exe dist/loopguard-hook.exe` 跑出 `LevelHook` 层 5~6 条断言且全 PASS；
`--exe dist/loopguard-hook.exe --descriptor loopguard-hook/conformance.json` 额外验出 `reasoning_loop` 终止语义。

### 阶段 4 · 交付「范例」而非「替插件跑」（模板 + 文档，不改工具）

1. 在 `conformance/` 提供 `templates/publish.yml`（GitHub Actions **范例**）：
   `build.sh --zip` → `ww3-conformance --exe dist/<id>-ext.exe --json-summary out.json`
   → 用 `out.json` 的 `conformance` 子对象更新注册表 `<id>.json` 并开 PR。
   模板里要显式标注「**可选步骤**：删掉这一段不影响发布」，并给出 `continue-on-error: true` 与
   「无 `conformance.json` 时只跑 A 层」的写法。
2. 在 `conformance/` 提供 `examples/conformance.json`（描述文件范例）+ `examples/README.md`（怎么拷进自己仓）。
3. `conformance/README.md` 增加「作为工具被其它仓调用」章节：`--exe` 用法、`conformance.json` 写法、摘要字段表、
   **明确写出「本工具不是门禁，注册表只校格式；加不加验证步骤由插件作者自己决定」**（用户 m02550 + m02729）。
4. `ext-registry/README.md` 的发布流程段补一句：`conformance` 字段可用 `ww3-conformance --json-summary` 产出，也可留空。

**本仓不做的事**（m02729 边界，写进 §0.1 与 README）：
- 不在主仓 `build.sh` / `temp/run_regress.sh` 里跑任何插件的准入验证；
- 不维护「19 个渠道的验证账」，不代插件仓提 PR；
- 不因某插件没跑而报错或阻止其发布。

验收：拿 `chatgpt-cli` 仓按模板**手工模拟**一遍 workflow 步骤（本仓不代跑），产出的 `conformance`
与现有注册表里那份**量级一致**（不必逐字相等，断言总数可能因新增 hook 层/摘要层而增加）；
把模板原样拷进一个不带 `conformance.json` 的插件仓，验证「A 层零配置也能跑、且不阻断发布」。

---

## 5. 兼容与回归口径

- **`--dist ../dist` 的全量结果必须不变**：改造完成后跑 `cd conformance && go run ./cmd/ww3-conformance --dist ../dist`，
  19 渠道的 PASS/WARN/SKIP 计数与改造前的基线**逐渠道相等**
  （新增的 hook 层只对 `kind=hook` 生效，不影响渠道插件）。
- **不改 `internal/wire` 与 `internal/mock` 的对外语义**（它们是「独立实现去校验契约」的价值所在，W3-36）。
- **不改 `main/protocol` 的任何一行**（`conformance` 刻意不复用主仓 framing，避免 18 份副本漂移）。
- 改造完成后 `temp/run_regress.sh` 里**不加** conformance 步骤（它不在 `build.sh` 批处理内，`AGENTS.md` §2-AO 已明确
  「conformance 不进批处理，它不是交付物」）；只在文档里给出跑法。
- **本仓永不代插件跑验证**（§0.1）：工具改造完成后，主仓的构建/回归/发版流程里**不出现**任何「对 19 个插件逐个跑
  conformance 并据此决定是否发版」的步骤；`temp/gen_registry.py` 现有 `load_conformance()`（从本机手跑报告抠计数）
  是**过渡期的临时做法**，改造后应改为「读插件仓自己提交的摘要，读不到就不写这个字段」。

---

## 6. 未决问题（实施时再定，先记录）

1. **描述文件的 schema 版本演进**：`schema:1` 是锚点，将来加字段一律 append-only；破坏性改动必须 `schema:2` 并让旧值报错（与注册表 `schema:2` 同一条纪律）。
2. **B 层「mock 路由写错导致假 PASS」的残余风险**：`chat.upstream` 只能证明「有请求到达」，不能证明「请求形状对」。
   是否给 `conformance.json` 加可选的 `expect_request`（断言上游收到的 body 含某些字段）？倾向**加**，但只在 `--strict` 下强制。
3. **`--exe` 多文件**：一次测多个插件时，`--json-summary` 是数组还是「每个插件一个文件」？倾向前者（数组），
   但注册表对接是「一插件一份」，所以 `--print-snippet` 只在单插件模式下可用。
4. **live 模式的凭据从哪来（CI 场景）**：现状 `--data/--auth`。若将来真要在 CI 里跑 live，需要秘密管理方案；
   本期**明确不做**，`mode: live` 保持人工。
5. **`ability.auth` 的取值**：六范式 `Type` 之一还是布尔？倾向取 `auth.methods` 里第一个 Type（`device_poll`/`loopback_callback`/`cdp_capture`/`form`/`credential_import`/`none`），
   与 `protocol/docs/AUTH_FLOW.md` 同一套词表，便于面板展示「这个渠道怎么登录」。
6. **（已定）非单 exe 插件的启动命令行**：用户 m02793 定案「`--exe` 给解释器、`--` 之后透传给 exe」，m02861 明确
   「`--` 前是 conformance 的参数，后面是 exe 的参数」——不再有 `--exe-arg` 这套第二写法（§3.1.1）。
   残留问题只有一个：`exe_sha256` 对解释器无意义 → 有 `exe_args` 时省略。

---

## 7. 与其它文档的关系（改动时同步）

| 文档 | 要同步的内容 |
|---|---|
| `docs/PLUGIN_REGISTRY.md` §2.1 | `conformance` 字段说明补「可由 `ww3-conformance --json-summary` 产出，可选」 |
| `docs/PLUGIN_REGISTRY.md` §7.1 | 发布流程补「可选：跑准入自检并把 `conformance` 写进 PR」 |
| `AGENTS.md` §2-AG（W3-36） | 新增「独立 CLI 工具形态」一段：`--exe`/`conformance.json`/`--json-summary`，以及「不是门禁」的定位 |
| `AGENTS.md` §2-AO（W3-41） | 保持「conformance 不进批处理」不变 |
| `conformance/README.md` | 新增「单插件用法（含非单 exe 插件：`--exe python.exe -- main.py`）」「描述文件」「摘要字段」「集成模板」四节 |
| `ext-registry/README.md` | 发布流程段补 `conformance` 字段的可选来源 |
| `temp/gen_registry.py` | `load_conformance()` 改为「优先读插件仓提交的摘要，读不到就不写该字段」，不再从主仓手跑报告抠计数 |
| `main/core/internal/api/web/app.js` | 已有「准入 pass/总数」徽章（`:833`）；改造后补「无数据则不显示」与 `ability` 能力矩阵展示，**不得因缺字段禁用安装** |
