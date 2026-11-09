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
#   ww3-conformance-{windows,linux,darwin}-{amd64,arm64}
```

## 三种用法

工具核心**零渠道知识**：渠道事实（mock 路由、夹具模型、能力缺口）全部来自描述文件。
删掉本仓的 `presets/` 目录，单插件模式照样完整可用。

### A 层：零配置（零网络）

只给可执行文件，检查契约骨架 + `capabilities.list` 与 manifest 的一致性：

```bash
ww3-conformance --exe /path/to/myext-ext.exe
```

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

[`templates/publish.yml`](templates/publish.yml) 是一份可拷贝的 workflow 范例。
**它只是范例，不是门禁**：加不加由你决定，不加同样能发布（注册表只校格式）。

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
