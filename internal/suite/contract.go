package suite

import (
	"context"
	"encoding/json"
	"fmt"
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

// cliCheck 检查四个必需子命令是否可用（不依赖 IPC）。
func cliCheck(ctx context.Context, env *harness.Env, rep *check.PluginReport) {
	type tc struct {
		id, name string
		args     []string
	}
	cases := []tc{
		{"cli.call", "子命令 call 存在", []string{"call", "--help"}},
		{"cli.stdio", "子命令 stdio 存在", []string{"stdio", "--help"}},
		{"cli.manifest", "子命令 manifest 存在", []string{"manifest", "--help"}},
		{"cli.ping", "子命令 ping 存在", []string{"ping", "--help"}},
	}
	for _, c := range cases {
		out, errb, code, err := env.RunCmd(ctx, c.args, nil, 15*time.Second)
		// --help 通常返回非 0（ErrHelp），只要能打出用法即视为存在
		body := string(out) + string(errb)
		st, detail := check.Pass, "可用"
		if err != nil && code == 0 {
			st, detail = check.Fail, "执行失败: "+err.Error()
		} else if !strings.Contains(body, "call") && !strings.Contains(body, "stdio") &&
			!strings.Contains(body, "manifest") && !strings.Contains(body, "ping") &&
			!strings.Contains(body, "用法") && !strings.Contains(body, "Usage") {
			st, detail = check.Warn, "无用法输出（可能未实现该子命令）"
		}
		rep.Add(check.Assertion{Level: check.LevelContract, ID: c.id, Name: c.name,
			Status: st, Detail: detail, Contract: "PLUGIN_CLI §2"})
	}

	// `call` / `stdio` 只接受 --data：多余开关必须报错
	out, errb, code, _ := env.RunCmd(ctx, []string{"call", "--model", "x"}, []byte("{}"), 15*time.Second)
	if code == 0 {
		rep.Add(check.Assertion{Level: check.LevelContract, ID: "cli.call-flags", Name: "call 只认 --data",
			Status: check.Warn, Detail: "call 接受了非契约开关 --model（应报错）",
			Contract: "PLUGIN_CLI §3"})
	} else {
		_ = out
		_ = errb
		rep.Add(check.Assertion{Level: check.LevelContract, ID: "cli.call-flags", Name: "call 只认 --data",
			Status: check.Pass, Detail: "非法开关被拒绝", Contract: "PLUGIN_CLI §3"})
	}
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
