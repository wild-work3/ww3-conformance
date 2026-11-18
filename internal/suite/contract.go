package suite

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/wild-work3/ww3-conformance/internal/check"
	"github.com/wild-work3/ww3-conformance/internal/harness"
	"github.com/wild-work3/ww3-conformance/internal/wire"
)

// contract 检查契约骨架层：manifest / 命令 / ping / initialize / capabilities / stdout 纯净度。
// 返回 manifest（不可用时为 nil）。
func contract(ctx context.Context, env *harness.Env, rep *check.PluginReport) map[string]any {
	m, err := env.Manifest(ctx)
	if err != nil {
		rep.Add(check.Assertion{Level: check.LevelContract, ID: "manifest.load", Name: "manifest 可获取",
			Status: check.Fail, Detail: err.Error(), Contract: "PLUGIN_CLI §2"})
		return nil
	}
	rep.Add(check.Assertion{Level: check.LevelContract, ID: "manifest.load", Name: "manifest 可获取",
		Status: check.Pass, Detail: fmt.Sprintf("id=%s version=%s kind=%s",
			strOr(m, "?", "id"), strOr(m, "?", "version"), strOr(m, "?", "kind")), Contract: "PLUGIN_CLI §2"})

	// 必需字段
	missing := []string{}
	for _, k := range []string{"id", "name", "version", "kind", "protocol"} {
		if _, ok := m[k]; !ok {
			missing = append(missing, k)
		}
	}
	st := check.Pass
	detail := "id/name/version/kind/protocol 齐备"
	if len(missing) > 0 {
		st, detail = check.Fail, "缺字段: "+strings.Join(missing, ", ")
	}
	rep.Add(check.Assertion{Level: check.LevelContract, ID: "manifest.fields", Name: "manifest 必需字段",
		Status: st, Detail: detail, Contract: "PLUGIN_CLI §2 / PLUGIN_PROTOCOL §3"})

	// 协议版本区间
	if min, ok := num(m, "protocol", "min"); ok {
		if max, ok2 := num(m, "protocol", "max"); ok2 {
			st := check.Pass
			detail = fmt.Sprintf("protocol [%v, %v]", min, max)
			if min > 1 || max < 1 {
				st, detail = check.Fail, fmt.Sprintf("协议版本区间 [%v,%v] 不含 1", min, max)
			}
			rep.Add(check.Assertion{Level: check.LevelContract, ID: "manifest.protocol", Name: "协议版本含 1",
				Status: st, Detail: detail, Contract: "PLUGIN_PROTOCOL §3"})
		}
	}

	// kind 必须是 channel 或 hook
	if k := str(m, "kind"); k != "channel" && k != "hook" {
		rep.Add(check.Assertion{Level: check.LevelContract, ID: "manifest.kind", Name: "kind 合法",
			Status: check.Fail, Detail: "kind=" + k + "（应为 channel/hook）", Contract: "PLUGIN_PROTOCOL §3"})
	} else {
		rep.Add(check.Assertion{Level: check.LevelContract, ID: "manifest.kind", Name: "kind 合法",
			Status: check.Pass, Detail: "kind=" + k, Contract: "PLUGIN_PROTOCOL §3"})
	}

	// 渠道实例声明（W3-43/W3-44）：要么 manifest 声明默认实例，要么声明实例由用户自建（multi_channels）。
	// 两者都没有时面板无从生成「添加账号」入口（也不允许拿 exe 名当渠道显示名），故判 FAIL。
	if str(m, "kind") == "channel" {
		ids, detail := map[string]bool{}, ""
		multi, _ := m["multi_channels"].(bool)
		defs, _ := m["default_channels"].([]any)
		for i, raw := range defs {
			ch, ok := raw.(map[string]any)
			if !ok || strings.TrimSpace(str(ch, "id")) == "" || strings.TrimSpace(str(ch, "name")) == "" {
				detail = fmt.Sprintf("default_channels[%d] 缺少 id/name", i)
				break
			}
			if ids[str(ch, "id")] {
				detail = "default_channels 重复 ID: " + str(ch, "id")
				break
			}
			ids[str(ch, "id")] = true
		}
		if detail == "" && len(defs) == 0 && !multi {
			detail = "既未声明 default_channels，也未声明 multi_channels"
		}
		status := check.Pass
		if detail != "" {
			// 工作区外的旧版参考 CLI 尚未采用 W3-43；它不在本项目 19 个独立仓构建清单里。
			if str(m, "id") == "glm" && strings.HasSuffix(strings.ToLower(env.Exe), "glm2cli.exe") {
				status = check.Warn
			} else {
				status = check.Fail
			}
		} else if multi {
			detail = fmt.Sprintf("实例由用户自建（multi_channels），预置实例 %d 个", len(defs))
		} else {
			detail = fmt.Sprintf("%d 个渠道实例有独立 ID/名称", len(defs))
		}
		rep.Add(check.Assertion{Level: check.LevelContract, ID: "manifest.default_channels", Name: "渠道实例声明",
			Status: status, Detail: detail, Contract: "PLUGIN_PROTOCOL §3.1.1"})
	}

	// execution 形态：渠道插件固定 call（W3-20）
	if d := str(m, "execution", "control"); d != "" && d != "call" {
		rep.Add(check.Assertion{Level: check.LevelContract, ID: "manifest.execution", Name: "渠道插件无常驻",
			Status: check.Warn, Detail: "execution.control=" + d + "（W3-20 要求 call）", Contract: "W3-20"})
	} else {
		rep.Add(check.Assertion{Level: check.LevelContract, ID: "manifest.execution", Name: "渠道插件无常驻",
			Status: check.Pass, Detail: "execution.control=call", Contract: "W3-20"})
	}

	// 必需命令：call / stdio / manifest / ping
	cliCheck(ctx, env, rep)

	// call 形态：ping / initialize / capabilities.list
	callPing(ctx, env, rep)

	// 未知方法必须回 not_found（不能挂死、不能 panic）
	unknownMethod(ctx, env, rep)

	// stdout 只能是协议帧：call 的输出必须能被 ParseFrames 完整解析
	stdoutPurity(ctx, env, rep)

	// manifest 声明的能力必须真的注册（抽查 lifecycle 三项）
	lifecycle(ctx, env, rep, m)

	return m
}

// cliCheck 检查 CLI 外壳（不依赖 IPC）：--help 详细手册、必需子命令、--version、用法错误退码。
//
// W3-57 定案：手册一律走 **stdout** 且 rc=0（与 --version 同口径，便于 `| less` / 脚本抓取）。
// 断言强度分两档：顶层手册 / 自定义命令手册 / --version / 非法命令 是硬断言（FAIL）；
// 四个契约命令逐条手册是软断言（WARN）—— 它们早已存在，但 D7（systemone/images）暂缓期间
// 不得让整渠道变红，缺一条契约命令仍值得看见，故 WARN 不 FAIL。
func cliCheck(ctx context.Context, env *harness.Env, rep *check.PluginReport) {
	add := func(id, name string, st check.Status, detail, contract string) {
		rep.Add(check.Assertion{Level: check.LevelContract, ID: id, Name: name,
			Status: st, Detail: detail, Contract: contract})
	}
	// run 返回 stdout / 合并正文 / 退出码；手册断言只看 stdout（走错流就算违背契约）
	run := func(args ...string) (so, all string, code int, err error) {
		body, eb, c, e := env.RunCmd(ctx, args, nil, 15*time.Second)
		return string(body), string(body) + string(eb), c, e
	}

	// ① 顶层 --help：详细手册必须在 stdout 里，且含关键小节
	so, _, code, err := run("--help")
	miss := []string{}
	for _, k := range []string{"用法", "命令", "契约命令", "开关", "双短横线"} {
		if !strings.Contains(so, k) {
			miss = append(miss, k)
		}
	}
	switch {
	case err != nil && code == 0:
		add("cli.help", "--help 详细手册", check.Fail, "执行失败: "+err.Error(), "PLUGIN_CLI §3/§9")
	case code != 0:
		add("cli.help", "--help 详细手册", check.Fail, fmt.Sprintf("rc=%d（--help 必须 rc=0）", code), "PLUGIN_CLI §3/§9")
	case strings.TrimSpace(so) == "":
		add("cli.help", "--help 详细手册", check.Fail, "stdout 为空（手册走到了 stderr）", "PLUGIN_CLI §3/§9")
	case len(miss) > 0:
		add("cli.help", "--help 详细手册", check.Fail, "手册缺小节: "+strings.Join(miss, ", "), "PLUGIN_CLI §3/§9")
	default:
		add("cli.help", "--help 详细手册", check.Pass, fmt.Sprintf("stdout %d 字节，含用法/命令/契约命令/开关/双短横线", len(so)), "PLUGIN_CLI §3/§9")
	}

	// ② -h / help 两个别名与 --help 同形（同一份手册）
	if so2, _, c2, _ := run("-h"); c2 != 0 || strings.TrimSpace(so2) == "" {
		add("cli.help.alias", "-h 等价 --help", check.Fail, fmt.Sprintf("rc=%d stdout=%d 字节", c2, len(so2)), "PLUGIN_CLI §3")
	} else if so3, _, c3, _ := run("help"); c3 != 0 || strings.TrimSpace(so3) == "" {
		add("cli.help.alias", "-h 等价 --help", check.Fail, fmt.Sprintf("help rc=%d stdout=%d 字节", c3, len(so3)), "PLUGIN_CLI §3")
	} else {
		add("cli.help.alias", "-h / help 等价 --help", check.Pass, "两种写法均走 stdout", "PLUGIN_CLI §3")
	}

	// ③ 四个必需子命令：手册存在即可（逐条软断言）
	for _, c := range []struct{ id, cmd string }{{"cli.call", "call"}, {"cli.stdio", "stdio"}, {"cli.manifest", "manifest"}, {"cli.ping", "ping"}} {
		so, _, code, err := run(c.cmd, "--help")
		st, detail := check.Pass, "可用"
		switch {
		case err != nil && code == 0:
			st, detail = check.Warn, "执行失败: "+err.Error()
		case code != 0 || strings.TrimSpace(so) == "":
			st, detail = check.Warn, fmt.Sprintf("该命令手册未走 stdout 或退出码非 0（rc=%d）", code)
		default:
			detail = fmt.Sprintf("手册 %d 字节走 stdout", len(so))
		}
		add(c.id, "子命令 "+c.cmd+" 存在", st, detail, "PLUGIN_CLI §2")
	}

	// ④ 逐命令手册：只对顶层手册里出现的命令断言（硬）
	// 「命令：」段的行首有两种形态（` * call  说明` 缺省命令带星号、`   accounts  说明` 三个空格），
	// 按缩进判段落会在第一行就 break（实测 kilo 因此误报「顶层手册未列出自定义命令」），改用正则扫行。
	customs := customCommands(so)
	if len(customs) == 0 {
		add("cli.help.cmd", "逐命令 --help", check.Warn, "顶层手册未列出自定义命令", "PLUGIN_CLI §9")
	} else {
		bad := []string{}
		for _, cmd := range customs[:min(8, len(customs))] { // 命令数上限保护：每个一次 spawn
			if so, _, code, _ := run(cmd, "--help"); code != 0 || strings.TrimSpace(so) == "" {
				bad = append(bad, fmt.Sprintf("%s(rc=%d,stdout=%dB)", cmd, code, len(so)))
			}
		}
		if len(bad) > 0 {
			add("cli.help.cmd", "逐命令 --help", check.Fail, "手册缺失或 rc≠0: "+strings.Join(bad, " "), "PLUGIN_CLI §9")
		} else {
			add("cli.help.cmd", "逐命令 --help", check.Pass, fmt.Sprintf("抽查 %d/%d 个自定义命令均有手册", min(8, len(customs)), len(customs)), "PLUGIN_CLI §9")
		}
	}

	// ⑤ --version 必须可用（W3-57 D4：以前只有 gateway/core 有，插件一律报未知开关）
	if so, _, code, _ := run("--version"); code != 0 || strings.TrimSpace(so) == "" {
		add("cli.version", "--version 自报身份", check.Fail, fmt.Sprintf("rc=%d stdout=%d 字节", code, len(so)), "PLUGIN_CLI §3")
	} else {
		add("cli.version", "--version 自报身份", check.Pass, strings.TrimSpace(so), "PLUGIN_CLI §3")
	}

	// ⑥ 非法命令必须非 0 退出码（约定 2 = usage_error）
	if _, _, code, _ := run("no-such-command-xyz"); code == 0 {
		add("cli.usage", "非法命令退码", check.Fail, "未知命令竟 rc=0", "PLUGIN_CLI §9")
	} else if code != 2 {
		add("cli.usage", "非法命令退码", check.Warn, fmt.Sprintf("rc=%d，约定用法错误为 2", code), "PLUGIN_CLI §9")
	} else {
		add("cli.usage", "非法命令退码", check.Pass, "rc=2", "PLUGIN_CLI §9")
	}

	// `call` / `stdio` 只接受 --data：多余开关必须报错
	_, _, code, _ = run("call", "--model", "x")
	if code == 0 {
		add("cli.call-flags", "call 只认 --data", check.Warn, "call 接受了非契约开关 --model（应报错）", "PLUGIN_CLI §3")
	} else {
		add("cli.call-flags", "call 只认 --data", check.Pass, "非法开关被拒绝", "PLUGIN_CLI §3")
	}
}

// contractCmds 是契约必需的四个子命令（PLUGIN_CLI §2），扫顶层手册时要排除。
var contractCmds = []string{"call", "stdio", "manifest", "ping"}

// cmdRowRe 匹配顶层手册「命令：」段的每一行（行首可选缩进 + 可选 `*` 缺省标记）。
var cmdRowRe = regexp.MustCompile(`(?m)^[ \t]*[*]?[ \t]+([a-z][a-z0-9_-]{1,20})[ \t]+\S`)

// customCommands 从顶层手册取自定义命令名（剔除四条契约命令，保持出现顺序）。
func customCommands(manual string) []string {
	i := strings.Index(manual, "命令：")
	if i < 0 {
		return nil
	}
	rest := manual[i+len("命令："):]
	if j := strings.Index(rest, "\n\n"); j > 0 {
		rest = rest[:j] // 命令段以空行结束
	}
	out := []string{}
	for _, m := range cmdRowRe.FindAllStringSubmatch(rest, -1) {
		if !hasStr(contractCmds, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

func hasStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// callPing 用 call 形态跑 ping，验证一次性进程形态可用。
func callPing(ctx context.Context, env *harness.Env, rep *check.PluginReport) {
	res, err := env.Call(ctx, "ping", nil, harness.DefaultTimeout)
	if err != nil {
		rep.Add(check.Assertion{Level: check.LevelContract, ID: "call.ping", Name: "call 形态 ping",
			Status: check.Fail, Detail: err.Error(), Contract: "PLUGIN_PROTOCOL §5.1"})
		return
	}
	if res.Err != nil {
		rep.Add(check.Assertion{Level: check.LevelContract, ID: "call.ping", Name: "call 形态 ping",
			Status: check.Fail, Detail: res.Err.Error(), Contract: "PLUGIN_PROTOCOL §5.1"})
		return
	}
	got := ""
	if s, ok := res.Obj["_raw"].(string); ok {
		got = s
	}
	// ping 的结果可能是字符串 "pong"（裸 JSON 字符串）
	var s string
	if json.Unmarshal(res.Raw, &s) == nil {
		got = s
	}
	st, detail := check.Pass, "pong"
	if got != "pong" {
		st, detail = check.Warn, "ping 结果不是 \"pong\"（实际: "+truncStr(string(res.Raw), 120)+"）"
	}
	rep.Add(check.Assertion{Level: check.LevelContract, ID: "call.ping", Name: "call 形态 ping",
		Status: st, Detail: detail, Contract: "PLUGIN_PROTOCOL §5.1"})
}

// unknownMethod 未知方法必须回 not_found 且进程正常退出。
func unknownMethod(ctx context.Context, env *harness.Env, rep *check.PluginReport) {
	res, err := env.Call(ctx, "definitely.not.a.method", map[string]any{}, harness.DefaultTimeout)
	if err != nil {
		rep.Add(check.Assertion{Level: check.LevelContract, ID: "call.unknown", Name: "未知方法回 not_found",
			Status: check.Fail, Detail: err.Error(), Contract: "PLUGIN_PROTOCOL §5"})
		return
	}
	if res.Err == nil {
		rep.Add(check.Assertion{Level: check.LevelContract, ID: "call.unknown", Name: "未知方法回 not_found",
			Status: check.Fail, Detail: "未返回错误（应回 kind=not_found）", Contract: "PLUGIN_PROTOCOL §5"})
		return
	}
	st, detail := check.Pass, "kind="+res.Err.Kind
	if res.Err.Kind != "not_found" {
		st, detail = check.Warn, "kind="+res.Err.Kind+"（契约要求 not_found）"
	}
	rep.Add(check.Assertion{Level: check.LevelContract, ID: "call.unknown", Name: "未知方法回 not_found",
		Status: st, Detail: detail, Contract: "PLUGIN_PROTOCOL §5"})
}

// stdoutPurity 验证 call 的 stdout 只有协议帧（harness 已 ParseFrames 成功即可）。
func stdoutPurity(ctx context.Context, env *harness.Env, rep *check.PluginReport) {
	frame := wire.InvokeFrames(wire.Call{Method: "ping"})
	stdout, stderr, _, err := env.RunCmd(ctx, []string{"call", "--data", env.DataDir}, frame, harness.DefaultTimeout)
	if err != nil {
		rep.Add(check.Assertion{Level: check.LevelContract, ID: "stdout.pure", Name: "stdout 只含协议帧",
			Status: check.Fail, Detail: err.Error(), Contract: "AGENTS §9"})
		return
	}
	frames, perr := wire.ParseFrames(stdout)
	st, detail := check.Pass, fmt.Sprintf("%d 帧全部可解析", len(frames))
	if perr != nil {
		st, detail = check.Fail, "stdout 混入非 JSONL 内容: "+perr.Error()
	} else if len(frames) == 0 {
		st, detail = check.Fail, "stdout 无任何协议帧"
	}
	// stderr 必须承载日志（不能把日志写 stdout）
	if len(stderr) == 0 && st == check.Pass {
		detail += "；stderr 为空（日志建议落 stderr/文件）"
	}
	rep.Add(check.Assertion{Level: check.LevelContract, ID: "stdout.pure", Name: "stdout 只含协议帧",
		Status: st, Detail: detail, Contract: "AGENTS §9"})
}

// lifecycle 检查 initialize / capabilities.list / health.check。
func lifecycle(ctx context.Context, env *harness.Env, rep *check.PluginReport, m map[string]any) {
	// initialize
	res, err := env.Call(ctx, "initialize", map[string]any{"protocol": 1}, harness.DefaultTimeout)
	st, detail := check.Pass, "ok"
	if err != nil || res.Err != nil {
		st, detail = check.Fail, errText(err, res)
	} else if id := str(res.Obj, "plugin_id"); id != "" && id != str(m, "id") {
		st, detail = check.Fail, "plugin_id 与 manifest 不一致: "+id
	}
	rep.Add(check.Assertion{Level: check.LevelContract, ID: "lifecycle.initialize", Name: "initialize",
		Status: st, Detail: detail, Contract: "PLUGIN_PROTOCOL §4"})

	// capabilities.list 必须与 manifest 一致
	res2, err2 := env.Call(ctx, "capabilities.list", nil, harness.DefaultTimeout)
	if err2 != nil || res2.Err != nil {
		rep.Add(check.Assertion{Level: check.LevelContract, ID: "lifecycle.caps", Name: "capabilities.list",
			Status: check.Fail, Detail: errText(err2, res2), Contract: "PLUGIN_PROTOCOL §4"})
	} else {
		ctrl := strSlice(res2.Obj, "control")
		conv := strSlice(res2.Obj, "conversation")
		mc := strSlice(m, "capabilities", "control")
		mv := strSlice(m, "capabilities", "conversation")
		var diffs []string
		for _, c := range mc {
			if !hasCap(ctrl, c) {
				diffs = append(diffs, "manifest 声明但未注册: "+c)
			}
		}
		for _, c := range mv {
			if !hasCap(conv, c) {
				diffs = append(diffs, "manifest 声明但未注册(会话面): "+c)
			}
		}
		st, detail := check.Pass, fmt.Sprintf("control=%d conversation=%d 与 manifest 一致", len(ctrl), len(conv))
		if len(diffs) > 0 {
			st, detail = check.Fail, strings.Join(diffs, "；")
		}
		rep.Add(check.Assertion{Level: check.LevelContract, ID: "lifecycle.caps", Name: "capabilities.list",
			Status: st, Detail: detail, Contract: "PLUGIN_PROTOCOL §4"})
	}

	// health.check（部分插件不声明，按 Skip）
	if hasCap(strSlice(m, "capabilities", "control"), "health.check") {
		// 必须带上夹具配置（含 base_url）：不带时插件按「未配置」自报 unconfigured，
		// 那是真实行为而不是契约违背 —— 误报过 cline 的 lifecycle.health。
		res3, err3 := env.CallWithConfig(ctx, "health.check", nil, harness.DefaultTimeout)
		st, detail := check.Pass, "ok"
		if err3 != nil || res3.Err != nil {
			st, detail = check.Fail, errText(err3, res3)
		} else if s := str(res3.Obj, "state"); s != "ok" && s != "degraded" && s != "down" {
			// 契约只约束取值域；unconfigured 属插件自定的扩展态，记为 WARN 并保留原文。
			st, detail = check.Warn, "state="+s+"（契约取值域 ok/degraded/down；插件扩展态）"
		}
		rep.Add(check.Assertion{Level: check.LevelContract, ID: "lifecycle.health", Name: "health.check",
			Status: st, Detail: detail, Contract: "PLUGIN_PROTOCOL §5.1"})
	}
}

// errText 提取错误文本。
func errText(err error, res *wire.Result) string {
	if err != nil {
		return err.Error()
	}
	if res != nil && res.Err != nil {
		return res.Err.Error()
	}
	return "未知错误"
}

func truncStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
