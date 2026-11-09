// Package check 定义准入检查的结果模型与状态机。
//
// 三态（不是二态）是刻意的：SKIP 只允许用于「契约允许该渠道不实现」的情形
// （未声明能力 / 明确写进 schema.gaps 的缺口）；无凭据、上游对抗规则变更、
// 真实上游不可达一律 FAIL —— 用户明确要求这类情况必须在报告里显式失败。
package check

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Status 是单条断言的状态。
type Status string

const (
	Pass Status = "PASS"
	Fail Status = "FAIL"
	Skip Status = "SKIP" // 仅「契约允许不实现」
	// Warn 表示「行为可疑但不违契约」（例如 usage 全 0 但上游确实可能不报）。
	Warn Status = "WARN"
)

// Level 是检查的分类层级（报告按此分组）。
type Level string

const (
	LevelContract  Level = "契约骨架" // manifest/call/stdio/ping/生命周期
	LevelControl   Level = "控制面"  // 声明的每个能力
	LevelChat      Level = "会话面"  // chat.generate 单轮
	LevelMultiTurn Level = "多轮对话" // 模拟真实场景的多轮上下文
	LevelSession   Level = "会话管理" // web 逆向渠道 conv-*
	LevelHook      Level = "拦截器"  // hook.stream 双向流（HOOK_PROTOCOL）
	LevelLive      Level = "真实上游" // 真实凭据 + flash 模型抽查
	LevelUsage     Level = "用量口径" // usage 字段口径一致性
)

// Mode 是测试运行模式。
type Mode string

const (
	// ModeOffline 用本地 mock 上游，验证协议与归一化逻辑（零真实请求）。
	ModeOffline Mode = "offline"
	// ModeLive 用真实凭据打真实上游，只跑 flash 类低消耗模型。
	ModeLive Mode = "live"
)

// Assertion 是一条断言结果。
type Assertion struct {
	Level  Level
	ID     string // 稳定 ID，便于跨版本对比（如 chat.multi_turn.context_recall）
	Name   string // 人读名称
	Status Status
	Detail string // 失败原因 / 实测值 / 关键证据
	// Contract 指向契约出处（文档 + 章节），便于失败时定位。
	Contract string
	Took     time.Duration
}

// PluginReport 是一个插件的完整报告。
type PluginReport struct {
	PluginID string
	Name     string
	Version  string
	Kind     string
	Exe      string
	Mode     Mode
	// Mode 说明为什么用这个模式（报告要写清，用户要求失败必须显式）。
	ModeReason string
	// Capabilities 是 manifest 声明的能力（报告展示用）。
	Control      []string
	Conversation []string
	// Blocked 非空表示整轮无法测试（无凭据 / 二进制缺失 / 上游不可达），
	// 报告中该插件全部断言记 FAIL 并附此原因。
	Blocked string
	Checks  []Assertion
	Started time.Time
	Ended   time.Time
	// Notes 是人工可读的补充（渠道特有约束、上游已知缺陷）。
	Notes []string
}

// Add 追加一条断言。
func (p *PluginReport) Add(a Assertion) {
	p.Checks = append(p.Checks, a)
}

// Addf 追加一条带格式化的断言。
func (p *PluginReport) Addf(level Level, id, name string, st Status, contract, format string, args ...any) {
	p.Add(Assertion{Level: level, ID: id, Name: name, Status: st, Contract: contract, Detail: fmt.Sprintf(format, args...)})
}

// Block 把整轮标记为无法测试：所有已知断言位置补 FAIL。
// blocked 的原因会同时写进 Detail，满足「必须明确报告失败」。
func (p *PluginReport) Block(reason string) {
	p.Blocked = reason
	p.Addf(LevelContract, "run.blocked", "整轮可执行性", Fail, "AGENTS.md §9 / 准入测试规范",
		"无法实测：%s", reason)
}

// Counts 统计各状态数量。
func (p *PluginReport) Counts() map[Status]int {
	m := map[Status]int{}
	for _, c := range p.Checks {
		m[c.Status]++
	}
	return m
}

// Verdict 给出插件级结论。
//
// 判定规则（用户要求：无凭据/上游变更必须明确失败）：
//   - 有 FAIL → 不合格（FAIL）
//   - 无 FAIL 但有 WARN → 有条件通过（WARN）
//   - 否则通过（PASS）
func (p *PluginReport) Verdict() Status {
	c := p.Counts()
	if c[Fail] > 0 {
		return Fail
	}
	if c[Warn] > 0 {
		return Warn
	}
	if len(p.Checks) == 0 {
		return Fail // 一条都没跑就是失败，不能算通过
	}
	return Pass
}

// ByLevel 按层级分组（保持层级声明顺序）。
func (p *PluginReport) ByLevel() []struct {
	Level Level
	Items []Assertion
} {
	order := []Level{LevelContract, LevelControl, LevelChat, LevelMultiTurn, LevelUsage, LevelSession, LevelHook, LevelLive}
	buckets := map[Level][]Assertion{}
	for _, c := range p.Checks {
		buckets[c.Level] = append(buckets[c.Level], c)
	}
	var out []struct {
		Level Level
		Items []Assertion
	}
	for _, l := range order {
		if items := buckets[l]; len(items) > 0 {
			out = append(out, struct {
				Level Level
				Items []Assertion
			}{l, items})
		}
	}
	// 未在 order 里的层级追加到末尾，避免丢结果
	var extra []Level
	for l := range buckets {
		found := false
		for _, o := range order {
			if o == l {
				found = true
				break
			}
		}
		if !found {
			extra = append(extra, l)
		}
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i] < extra[j] })
	for _, l := range extra {
		out = append(out, struct {
			Level Level
			Items []Assertion
		}{l, buckets[l]})
	}
	return out
}

// Failures 返回全部 FAIL 断言（汇总用）。
func (p *PluginReport) Failures() []Assertion {
	var out []Assertion
	for _, c := range p.Checks {
		if c.Status == Fail {
			out = append(out, c)
		}
	}
	return out
}

// Summary 一行摘要（列表用）。
func (p *PluginReport) Summary() string {
	c := p.Counts()
	var b strings.Builder
	fmt.Fprintf(&b, "%s", p.Verdict())
	for _, s := range []Status{Pass, Warn, Fail, Skip} {
		if c[s] > 0 {
			fmt.Fprintf(&b, " %s=%d", s, c[s])
		}
	}
	return b.String()
}
