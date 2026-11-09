// summary.go 产出「可直接贴进插件注册表」的摘要（W3-51 §3.3）。
//
// 定位（用户 m02550 / m02729，PLAN §0.1）：**这是作者自证 + 读者参考，不是准入门禁**。
// 注册表只有 repo/version/file/sha256 是必要信息；conformance/capabilities 全部可选，
// 有就多显示一点，没有也照常安装。
//
// 因此本文件只做「把已有断言结果换个形状」，绝不新增探测请求、绝不写凭据。
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/wild-work3/ww3-conformance/internal/check"
)

// SummarySchema 是摘要格式的版本（前向兼容锚点）。
const SummarySchema = 1

// Summary 是注册表可用的摘要。
type Summary struct {
	Schema     int      `json:"schema"`
	ID         string   `json:"id"`
	Name       string   `json:"name,omitempty"`
	Version    string   `json:"version,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	ExeSHA256  string   `json:"exe_sha256,omitempty"`
	ExeArgs    []string `json:"exe_args,omitempty"`
	Mode       string   `json:"mode"`
	At         string   `json:"at"`
	Verdict    string   `json:"verdict"`
	Counts     Counts   `json:"counts"`
	Conformance *Conformance `json:"conformance,omitempty"`
	Capabilities *Capabilities `json:"capabilities,omitempty"`
	Ability    map[string]any `json:"ability,omitempty"`
	Note       string   `json:"note"`
	Failures   []Failure `json:"failures,omitempty"`
}

// Counts 是四态计数。
type Counts struct {
	Pass int `json:"pass"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
	Skip int `json:"skip"`
}

// Conformance 就是注册表 `<id>.json` 里那个 `conformance` 字段（形状完全一致）。
type Conformance struct {
	Pass int    `json:"pass"`
	Warn int    `json:"warn"`
	Fail int    `json:"fail"`
	Mode string `json:"mode"`
	At   string `json:"at"`
}

// Capabilities 取自 capabilities.list 的**实测返回**（不是 manifest 声明）。
type Capabilities struct {
	Control      []string `json:"control,omitempty"`
	Conversation []string `json:"conversation,omitempty"`
}

// Failure 是一条失败明细。
type Failure struct {
	Level  string `json:"level"`
	ID     string `json:"id"`
	Detail string `json:"detail"`
}

// noteOffline 是 offline 模式必须写进摘要的免责声明（PLAN §2 硬约束）。
const noteOffline = "offline 模式：仅验证契约与归一化逻辑，未访问真实上游；上游对抗算法/风控/凭据有效性未验证。"

// noteLive 是 live 模式的口径说明。
const noteLive = "live 模式：含真实上游抽查（每插件默认 1 轮、flash 类低消耗模型）；覆盖有限，不代表全部模型/账号可用。"

// Summaries 把整份 Bundle 转成摘要数组（每插件一份）。
//
// 之所以是数组：一次可测多个插件；但注册表对接是「一插件一份」，
// 所以 --print-snippet 只在单插件模式下有意义（PLAN §6.3）。
func (b *Bundle) Summaries(exeArgs []string) []Summary {
	out := make([]Summary, 0, len(b.Plugins))
	for _, p := range b.Plugins {
		out = append(out, b.one(p, exeArgs))
	}
	return out
}

// WriteSummary 写摘要文件。
//
// 单插件写对象（便于直接贴进注册表），多插件写数组。
func (b *Bundle) WriteSummary(path string) error {
	sums := b.Summaries(nil)
	var doc any = sums
	if len(sums) == 1 {
		doc = sums[0]
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// Snippet 把摘要渲染成「可直接粘进 <id>.json」的片段（单插件模式才有意义）。
func (b *Bundle) Snippet() string {
	if len(b.Plugins) != 1 {
		return ""
	}
	s := b.one(b.Plugins[0], nil)
	doc := map[string]any{}
	if s.Conformance != nil {
		doc["conformance"] = s.Conformance
	}
	if s.Capabilities != nil {
		doc["capabilities"] = s.Capabilities
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return ""
	}
	return string(raw)
}

// one 把单个插件报告转成摘要。
func (b *Bundle) one(p *check.PluginReport, exeArgs []string) Summary {
	c := p.Counts()
	s := Summary{
		Schema: SummarySchema,
		ID:     p.PluginID, Name: p.Name, Version: p.Version, Kind: p.Kind,
		ExeArgs: exeArgs,
		Mode:    string(b.Mode),
		At:      p.Ended.UTC().Format(time.RFC3339),
		Verdict: string(p.Verdict()),
		Counts: Counts{
			Pass: c[check.Pass], Warn: c[check.Warn], Fail: c[check.Fail], Skip: c[check.Skip],
		},
		// 注册表的 conformance 只记计数与口径，不记 SKIP（SKIP 是「契约允许不实现」，不是成绩）
		Conformance: &Conformance{
			Pass: c[check.Pass], Warn: c[check.Warn], Fail: c[check.Fail],
			Mode: string(b.Mode), At: p.Ended.Format("2006-01-02"),
		},
		Ability: deriveAbility(p),
		Note:    noteOf(b.Mode),
	}
	// capabilities 只在真有内容时写（空对象会让注册表校验与面板显示都变啰嗦）
	if len(p.Control) > 0 || len(p.Conversation) > 0 {
		s.Capabilities = &Capabilities{Control: p.Control, Conversation: p.Conversation}
	}
	for _, f := range p.Failures() {
		s.Failures = append(s.Failures, Failure{Level: string(f.Level), ID: f.ID, Detail: f.Detail})
	}
	return s
}

func noteOf(m check.Mode) string {
	if m == check.ModeLive {
		return noteLive
	}
	return noteOffline
}

// abilityKeyToAssertion 把 ability 键映射到「能证明它被测过」的断言 ID。
//
// 三态语义（重要）：断言**不存在** = 未测（A 层零配置跳过了控制面），
// 不能当成「不支持」。未测的键一律**不写**，而不是写 false——
// 否则面板会把「没测」展示成「这渠道没有该能力」，比不显示更误导。
var abilityKeyToAssertion = map[string][]string{
	"chat":            {"stream.events", "chat.call"},
	"models":          {"models.list"},
	"accounts":        {"accounts.list"},
	"credits":         {"credits.get"},
	"checkin":         {"accounts.checkin"},
	"resource_detail": {"accounts.resource_detail"},
	"stats":           {"stats.get"},
	"session":         {"session.schema"},
}

// deriveAbility 从断言结果派生「这个插件能做什么」的矩阵（面板/商店展示用）。
//
// 纯函数：只读 Checks，不发请求。取值三态：
//   - true  对应断言 PASS/WARN/SKIP（SKIP = 契约允许不实现，算「测过且不违背」）
//   - false 对应断言 FAIL（宁可少报能力，也不要把坏的能力标成可用）
//   - 不出现 未测（没有对应断言，例如 A 层跳过了控制面）
func deriveAbility(p *check.PluginReport) map[string]any {
	byID := map[string]check.Assertion{}
	for _, a := range p.Checks {
		// 同 ID 多条时取最差状态（FAIL 优先），避免「一条 PASS 掩盖一条 FAIL」
		if old, ok := byID[a.ID]; !ok || worse(a.Status, old.Status) {
			byID[a.ID] = a
		}
	}
	out := map[string]any{}
	for key, ids := range abilityKeyToAssertion {
		worst, tested := check.Status(""), false
		for _, id := range ids {
			if a, found := byID[id]; found {
				if !tested || worse(a.Status, worst) {
					worst = a.Status
				}
				tested = true
			}
		}
		if tested {
			out[key] = worst != check.Fail
		}
	}
	// images / systemone 按 manifest 声明的能力推导（不靠断言：它们是非流式可选能力，
	// 未声明时 core 直接回「渠道不支持」，无需测试）。声明了才算 true，没声明就不写。
	if hasCap(p.Control, "images.generate") {
		out["images"] = true
	}
	if hasCap(p.Control, "systemone.decide") {
		out["systemone"] = true
	}
	// auth 取六范式之一（与 protocol/docs/AUTH_FLOW.md 同一套词表），便于面板显示「这个渠道怎么登录」
	if a, found := byID["auth.methods"]; found && a.Status != check.Fail {
		if t := firstAuthType(p); t != "" {
			out["auth"] = t
		}
	}
	return out
}

// firstAuthType 从 auth.methods 断言明细里取第一个范式类型。
//
// 断言的 Detail 里带 `types=[...]`（见 control.go），这里做最小解析；
// 解析不到就不写该键（不臆造登录方式）。
func firstAuthType(p *check.PluginReport) string {
	for _, a := range p.Checks {
		if a.ID != "auth.methods" {
			continue
		}
		s := a.Detail
		i := indexOf(s, "types=[")
		if i < 0 {
			return ""
		}
		s = s[i+len("types=["):]
		if j := indexOf(s, "]"); j >= 0 {
			s = s[:j]
		}
		for k, part := range splitComma(s) {
			if k == 0 {
				return part
			}
		}
	}
	return ""
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		if r != ' ' && r != '"' {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	sort.SliceStable(out, func(i, j int) bool { return false })
	return out
}

// worse 报告 a 是否比 b 更差（FAIL > WARN > SKIP > PASS）。
func worse(a, b check.Status) bool {
	return rank(a) > rank(b)
}

func rank(s check.Status) int {
	switch s {
	case check.Fail:
		return 3
	case check.Warn:
		return 2
	case check.Skip:
		return 1
	default:
		return 0
	}
}

func hasCap(list []string, name string) bool {
	for _, c := range list {
		if c == name {
			return true
		}
	}
	return false
}

var _ = fmt.Sprintf
