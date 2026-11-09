// Package report 把准入测试结果渲染成 markdown 与 JSON。
//
// 报告要求（用户 m01897）：标准化、逐渠道、失败必须显式可见。
// 因此每个插件一节，按层级分表，失败项在「失败汇总」里再列一次，
// 便于人工直接扫到「为什么没通过」。
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"strings"
	"time"

	"github.com/wild-work3/ww3-conformance/internal/check"
)

// Bundle 是一次完整运行的结果。
type Bundle struct {
	Started time.Time
	Ended   time.Time
	Mode    check.Mode
	// Env 是运行环境摘要（go 版本、dist 目录等）。
	Env     []string
	Plugins []*check.PluginReport
}

// New 创建 Bundle。
func New(mode check.Mode) *Bundle {
	return &Bundle{Started: time.Now(), Mode: mode}
}

// Add 追加一个插件报告。
func (b *Bundle) Add(p *check.PluginReport) { b.Plugins = append(b.Plugins, p) }

// Finish 结束计时。
func (b *Bundle) Finish() { b.Ended = time.Now() }

// Totals 汇总各状态计数。
func (b *Bundle) Totals() map[check.Status]int {
	out := map[check.Status]int{}
	for _, p := range b.Plugins {
		c := p.Counts()
		for k, v := range c {
			out[k] += v
		}
	}
	return out
}

// Verdict 返回整体判定。
func (b *Bundle) Verdict() check.Status {
	if len(b.Plugins) == 0 {
		return check.Fail
	}
	worst := check.Pass
	for _, p := range b.Plugins {
		switch p.Verdict() {
		case check.Fail:
			return check.Fail
		case check.Warn:
			worst = check.Warn
		}
	}
	return worst
}

// WriteJSON 写出机器可读结果。
func (b *Bundle) WriteJSON(path string) error {
	doc := map[string]any{
		"started": b.Started.Format(time.RFC3339),
		"ended":   b.Ended.Format(time.RFC3339),
		"mode":    string(b.Mode),
		"env":     b.Env,
		"totals":  b.Totals(),
		"verdict": string(b.Verdict()),
		"plugins": b.Plugins,
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// WriteMarkdown 写出人读报告。
func (b *Bundle) WriteMarkdown(path string) error {
	md := b.Markdown()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(md), 0o644)
}

// Markdown 渲染报告正文。
func (b *Bundle) Markdown() string {
	var sb strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&sb, f, a...) }

	w("# 渠道插件准入测试报告\n\n")
	w("| 项 | 值 |\n|---|---|\n")
	w("| 运行模式 | %s |\n", b.Mode)
	w("| 开始 | %s |\n", b.Started.Format("2006-01-02 15:04:05"))
	w("| 耗时 | %s |\n", b.Ended.Sub(b.Started).Round(time.Millisecond))
	w("| 插件数 | %d |\n", len(b.Plugins))
	t := b.Totals()
	w("| 断言合计 | PASS %d / FAIL %d / WARN %d / SKIP %d |\n",
		t[check.Pass], t[check.Fail], t[check.Warn], t[check.Skip])
	w("| **总判定** | **%s** |\n\n", b.Verdict())
	// 顶层提示：避免读者把「对话成功」误读成真实上游可用
	if b.Mode == check.ModeOffline {
		w("> ⚠ 本报告为 **offline** 模式：控制面/会话面断言全部跑在**本地 mock 上游**上，" +
			"不访问任何真实上游。报告的「对话成功」验证的是插件→契约→请求发出，" +
			"**不是**上游对抗算法/风控/凭据有效性的验证；真实上游结论见 live 报告。\n\n")
	}
	if len(b.Env) > 0 {
		w("运行环境：\n\n")
		for _, e := range b.Env {
			w("- %s\n", e)
		}
		w("\n")
	}

	// 总览表
	w("## 一、总览\n\n")
	w("| 插件 | 版本 | 模式 | 判定 | PASS | FAIL | WARN | SKIP | 说明 |\n")
	w("|---|---|---|---|---|---|---|---|---|\n")
	for _, p := range b.Plugins {
		c := p.Counts()
		note := ""
		if p.Blocked != "" {
			note = "阻塞：" + esc(p.Blocked)
		} else if p.ModeReason != "" {
			note = esc(p.ModeReason)
		}
		w("| %s | %s | %s | **%s** | %d | %d | %d | %d | %s |\n",
			p.PluginID, p.Version, p.Mode, p.Verdict(),
			c[check.Pass], c[check.Fail], c[check.Warn], c[check.Skip], note)
	}
	w("\n")

	// 失败汇总（最重要，放前面）
	w("## 二、失败汇总\n\n")
	anyFail := false
	for _, p := range b.Plugins {
		fs := p.Failures()
		if len(fs) == 0 {
			continue
		}
		anyFail = true
		w("### %s（%d 项失败）\n\n", p.PluginID, len(fs))
		w("| 层级 | 断言 | 契约 | 详情 |\n|---|---|---|---|\n")
		for _, a := range fs {
			w("| %s | %s | %s | %s |\n", a.Level, a.Name, a.Contract, esc(a.Detail))
		}
		w("\n")
	}
	if !anyFail {
		w("无失败项。\n\n")
	}

	// 覆盖范围与限制：离线全绿不等于真实上游可用，必须写清楚，否则读者会误读
	w("## 三、覆盖范围与限制\n\n")
	w("| 项 | 说明 |\n|---|---|\n")
	if b.Mode == check.ModeOffline {
		w("| 运行模式 | **纯 mock**（offline）：全部断言跑在本地 mock 上游上，**零真实请求** |\n")
		w("| 已验证 | 契约骨架、控制面全部声明能力、会话面 chat.generate、多轮历史下发、usage 口径、web 逆向渠道的会话管理命令 |\n")
		w("| 未验证 | 真实上游的认证有效性、对抗规则（签名/指纹/风控）、上游真实 usage 字段、上游错误分类；上游若已风控/对抗算法失效，本报告**不会**发现 |\n")
		w("| 如何补验 | `--mode live --data <凭据目录> [--auth <文件名>] [--model <模型ID>]`，可配 `WW3_LIVE_PROXY` 走代理 |\n")
	} else {
		w("| 运行模式 | live：先跑完全部 mock 断言，再用真实凭据对真实上游做**对话抽查**（flash 类低消耗模型、短 prompt、max_tokens=64） |\n")
		w("| 真实请求数 | 默认**每渠道 1 轮**（避风控）；多轮真实上下文未验证，需显式 `--live-turns 2` |\n")
		w("| 凭据来源 | `--auth` 显式指定单份；或 `--data` 按渠道前缀自动发现（只取最新一份，不重试多份） |\n")
	}
	w("| 无凭据的渠道 | 按用户要求**显式 FAIL**（`live.auth`），不静默跳过；离线契约结果仍然有效 |\n")
	w("| 模型覆盖 | 每渠道只抽查 1 个 flash 类模型，不逐模型验证（用户 m01897 要求控制消耗） |\n")
	w("\n")

	// 逐插件明细
	w("## 四、逐插件明细\n\n")
	for _, p := range b.Plugins {
		w("### %s %s\n\n", p.PluginID, p.Version)
		if p.Name != "" || p.Kind != "" {
			w("- 名称：%s；类型：%s；可执行文件：`%s`\n", p.Name, p.Kind, p.Exe)
		}
		if p.Mode == check.ModeOffline {
			w("- 本次运行：**纯 mock**（offline）——本地 mock 上游，**零真实请求**\n")
		} else if p.Mode == check.ModeLive {
			w("- 本次运行：live（mock 断言 + 真实上游单轮抽查）\n")
		}
		if p.Blocked != "" {
			w("- **阻塞原因**：%s\n", esc(p.Blocked))
		}
		w("- 控制面能力（%d）：%s\n", len(p.Control), strings.Join(p.Control, ", "))
		w("- 会话面能力（%d）：%s\n", len(p.Conversation), strings.Join(p.Conversation, ", "))
		w("- 耗时：%s\n", p.Ended.Sub(p.Started).Round(time.Millisecond))
		for _, n := range p.Notes {
			w("- 备注：%s\n", esc(n))
		}
		w("\n")

		for _, grp := range p.ByLevel() {
			w("#### %s\n\n", grp.Level)
			w("| 结果 | 断言 | 详情 | 契约 |\n|---|---|---|---|\n")
			for _, a := range grp.Items {
				w("| %s | %s | %s | %s |\n", a.Status, a.Name, esc(a.Detail), a.Contract)
			}
			w("\n")
		}
	}

	// 附：WARN/SKIP 明细（不阻断，但必须可查，否则「全部 WARN」等于什么都没说）
	w("## 五、告警与跳过明细\n\n")
	anySoft := false
	for _, p := range b.Plugins {
		var soft []check.Assertion
		for _, a := range p.Checks {
			if a.Status == check.Warn || a.Status == check.Skip {
				soft = append(soft, a)
			}
		}
		if len(soft) == 0 {
			continue
		}
		anySoft = true
		w("### %s（%d 项）\n\n", p.PluginID, len(soft))
		w("| 结果 | 断言 | 详情 |\n|---|---|---|\n")
		for _, a := range soft {
			w("| %s | %s | %s |\n", a.Status, a.Name, esc(a.Detail))
		}
		w("\n")
	}
	if !anySoft {
		w("无告警项。\n\n")
	}

	w("---\n\n")
	w("判定口径：PASS=通过；FAIL=契约违规或无法验证（含无凭据、上游对抗规则变更、上游不可达）；" +
		"WARN=非阻断性问题；SKIP=契约允许不实现（能力未声明 / schema.gaps 已声明缺口）。\n")
	return sb.String()
}

func esc(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
