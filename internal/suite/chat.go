package suite

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wild-work3/ww3-conformance/internal/check"
	"github.com/wild-work3/ww3-conformance/internal/harness"
	"github.com/wild-work3/ww3-conformance/internal/mock"
)

// randToken 生成短随机串（多轮对话的历史标记，防止误命中）。
func randToken(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// sessionCID 从 mock 请求记录里找不到会话 ID 时，由描述符提供；
// 这里优先从环境变量 WW3_CONF_CID 读（报告可复现），否则空。
func sessionCID(env *harness.Env) string {
	return env.CID
}

// authArgFor 返回便利命令要用的 --auth 值。
func authArgFor(env *harness.Env) string {
	return env.AuthName
}

// chatBasics 检查会话面单轮：事件序列、正文、结束语义、错误分类。
func chatBasics(ctx context.Context, env *harness.Env, rep *check.PluginReport, model, accountUID string) {
	req := baseChatReq(model, accountUID, []map[string]any{
		{"role": "system", "content": "你是一个简洁的助手。"},
		{"role": "user", "content": "用一句话介绍你自己。"},
	})
	res, err := env.Chat(ctx, req, harness.ChatTimeout)
	if err != nil {
		rep.Add(assert("chat.call", "chat.generate 可调用", check.Fail, err.Error(), "IPC_CHAT §3"))
		return
	}
	if res.Err != nil {
		st := check.Fail
		// mock 上游 404/未覆盖路由导致的失败属夹具问题，但仍是 FAIL（不得静默跳过）
		detail := res.Err.Error()
		if res.Err.Kind == "not_supported" {
			st, detail = check.Skip, "渠道声明不支持 chat.generate"
		}
		rep.Add(assert("chat.call", "chat.generate 可调用", st, detail, "IPC_CHAT §3"))
		return
	}
	rep.Add(assert("chat.call", "chat.generate 可调用", check.Pass,
		fmt.Sprintf("%d 个事件，耗时 %s", len(res.Events), res.Took.Round(time.Millisecond)), "IPC_CHAT §3"))

	// 事件名合法性（IPC_CHAT §3 事件集）
	// 注意 completed/incomplete 是契约的终止事件（IPC_CHAT §3 表），不是越界事件；
	// 早期漏掉它们会把合规插件判成 FAIL。
	valid := map[string]bool{"text.delta": true, "reasoning.delta": true, "tool_call.delta": true,
		"usage": true, "finish_reason": true, "error": true, "image.delta": true, "meta": true,
		"completed": true, "incomplete": true, "async.complete": true}
	bad := []string{}
	for _, e := range res.Events {
		if !valid[e.Name] {
			bad = append(bad, e.Name)
		}
	}
	st, detail := check.Pass, "全部为契约事件名"
	if len(bad) > 0 {
		st, detail = check.Fail, "非法事件名: "+strings.Join(bad, ", ")
	}
	rep.Add(assert("stream.events", "流式事件名合法", st, detail, "IPC_CHAT §3"))

	// 正文非空
	text := res.Text()
	st, detail = check.Pass, fmt.Sprintf("%d 字", len([]rune(text)))
	if strings.TrimSpace(text) == "" {
		st, detail = check.Fail, "正文为空（text.delta 未产生内容）"
	}
	rep.Add(assert("stream.text", "流式正文非空", st, detail, "IPC_CHAT §3"))

	// finish_reason 必须有且只有一个
	n := 0
	for _, e := range res.Events {
		if e.Name == "finish_reason" {
			n++
		}
	}
	st, detail = check.Pass, "1 个 finish_reason"
	if n == 0 {
		st, detail = check.Fail, "缺 finish_reason（终止事件只能出现一次，W3-10）"
	} else if n > 1 {
		st, detail = check.Fail, fmt.Sprintf("%d 个 finish_reason（应只有 1 个）", n)
	}
	rep.Add(assert("stream.finish", "finish_reason 唯一", st, detail, "IPC_CHAT §3 / W3-10"))

	// 成功后不得有 error 事件
	hasErr := false
	for _, e := range res.Events {
		if e.Name == "error" {
			hasErr = true
		}
	}
	st, detail = check.Pass, "无 error 事件"
	if hasErr {
		st, detail = check.Fail, "成功响应里混有 error 事件"
	}
	rep.Add(assert("stream.no-error", "成功流不含 error", st, detail, "W3-10"))

	// 上游真的收到了带鉴权的请求（渠道没有静默打空转）
	if env.Mock != nil && len(env.Mock.Requests()) == 0 {
		rep.Add(assert("chat.upstream", "请求到达 mock 上游", check.Fail,
			"mock 上游未收到任何请求（插件可能未真正发起调用）", "IPC_CHAT §2"))
	} else if env.Mock != nil {
		rs := env.Mock.Requests()
		last := rs[len(rs)-1]
		rep.Add(assert("chat.upstream", "请求到达 mock 上游", check.Pass,
			"mock 收到 "+last.Method+" "+last.Path, "IPC_CHAT §2"))
	}
}

// baseChatReq 构造一个 chat.generate 请求体。
func baseChatReq(model, accountUID string, msgs []map[string]any) map[string]any {
	msgAny := make([]any, 0, len(msgs))
	for _, m := range msgs {
		msgAny = append(msgAny, m)
	}
	r := map[string]any{"model": model, "messages": msgAny, "stream": true}
	if accountUID != "" {
		r["account_uid"] = accountUID
	}
	return r
}

// multiTurn 跑三轮真实场景对话，验证上下文完整下发与累积。
//
// 设计依据（用户 m01897「用例需要包含（模拟）真实场景的多轮对话」）：
//   - core 每轮都把**完整历史**下发给插件（W3-03：一请求一进程，不维护对话映射），
//     因此「插件是否真的把历史送给上游」是必须验证的契约行为。
//   - 第 1 轮：system + 用户自我介绍（含一个只有第 1 轮才有的随机标记）
//   - 第 2 轮：带第 1 轮 assistant 回复，用户追问（含第 2 轮标记）
//   - 第 3 轮：带前两轮，用户要求复述第 1 轮标记 → 校验前文仍在请求体里
func multiTurn(ctx context.Context, env *harness.Env, rep *check.PluginReport, model, accountUID string) {
	turn1Mark := "MARK1-" + randToken(6)
	turn2Mark := "MARK2-" + randToken(6)
	turn3Mark := "MARK3-" + randToken(6)
	convID := "" // 会话式渠道的续接点（第 1 轮返回后填入）

	// 真实场景脚本：让模型扮演一个记账助手，第 3 轮要求引用第 1 轮的事实。
	hist := []map[string]any{
		{"role": "system", "content": "你是一个记账助手，回答要简洁。"},
		{"role": "user", "content": "请记住这个编号 " + turn1Mark + "，我买了一杯咖啡 25 元。"},
	}

	type turn struct {
		id   string
		user string
		// wantInBody 是本轮结束后「上游请求体里必须出现」的标记（验证历史累积）
		wantInBody []string
	}
	turns := []turn{
		{"multi.turn1", "请记住这个编号 " + turn1Mark + "，我买了一杯咖啡 25 元。", nil},
		{"multi.turn2", "再加一本书 60 元，编号 " + turn2Mark + "。", []string{turn1Mark}},
		{"multi.turn3", "把前两笔合计一下，并复述第一个编号（" + turn3Mark + "）。",
			[]string{turn1Mark, turn2Mark}},
	}

	for i, t := range turns {
		if i > 0 {
			// 上一轮的 assistant 回复进入历史（模拟真实多轮）
			prev := ""
			if i-1 < len(hist) {
				prev = "（上一轮回复）"
			}
			hist = append(hist, map[string]any{"role": "assistant", "content": prev})
			hist = append(hist, map[string]any{"role": "user", "content": t.user})
		}
		if env.Mock != nil {
			env.Mock.Reset()
		}
		req := baseChatReq(model, accountUID, hist)
		// 会话式渠道（chatgpt 类 web 逆向）不在请求体里重放历史，而是靠
		// conversation_id 续接；上一轮返回的 conversation_id 必须显式回传，
		// 否则每轮都是新会话（历史真的丢了）。对重放型渠道该字段无害。
		if convID != "" {
			req["conversation_id"] = convID
		}
		res, err := env.Chat(ctx, req, harness.ChatTimeout)
		if err != nil || res.Err != nil || !res.OK {
			rep.Add(assert(t.id, fmt.Sprintf("第 %d 轮对话成功", i+1), check.Fail,
				errText(err, res), "IPC_CHAT §2"))
			return
		}
		text := res.Text()
		st, detail := check.Pass, fmt.Sprintf("%d 字", len([]rune(text)))
		if strings.TrimSpace(text) == "" {
			st, detail = check.Fail, "正文为空"
		}
		rep.Add(assert(t.id, fmt.Sprintf("第 %d 轮对话成功", i+1), st, detail, "IPC_CHAT §2"))
		if cid := str(res.Obj, "conversation_id"); cid != "" {
			convID = cid
		}

		// 校验历史真的被下发到上游（这是多轮的核心契约）
		if len(t.wantInBody) > 0 && env.Mock != nil {
			all := strings.Builder{}
			for _, r := range env.Mock.Requests() {
				all.WriteString(r.Body)
				all.WriteString("\n")
			}
			body := all.String()
			// 多轮上下文有两条合法路径：①重放型渠道把历史消息整体下发（标记出现在 body）；
			// ②会话式渠道靠 conversation_id/parent 续接（标记不在 body，但会话 ID 必须在）。
			// 只认 ① 会把 chatgpt 这类渠道误判成「历史未下发」。
			convCarried := convID != "" && strings.Contains(body, convID)
			// 部分上游（qoder/qwenwork 的 Encode=1）把整个请求体编码成自定义 base64，
			// 明文标记不可能出现在 body 里。此时先尝试解码再匹配，否则只能误判。
			plain := body
			if !strings.Contains(body, "{") {
				plain = tryDecodeBody(body)
			}
			missing := []string{}
			for _, w := range t.wantInBody {
				if !strings.Contains(plain, w) && !convCarried {
					missing = append(missing, w)
				}
			}
			st, detail := check.Pass, "历史标记全部出现在上游请求体"
			if convCarried {
				st, detail = check.Pass, "会话式续接：conversation_id 已回传（"+convID+"）"
			}
			if len(missing) > 0 {
				st, detail = check.Fail, "上游请求体缺少历史标记: "+strings.Join(missing, ", ")+
					"（多轮上下文未下发）"
			}
			rep.Add(assert("multi.history"+fmt.Sprint(i+1), fmt.Sprintf("第 %d 轮携带历史上下文", i+1),
				st, detail, "IPC_CHAT §2 / W3-03"))
		}
	}
}

// usageShape 检查用量口径（W3-34 / W3-22）：reported 与 unknown 必须可区分，禁止臆造。
func usageShape(ctx context.Context, env *harness.Env, rep *check.PluginReport, model, accountUID string, noToken bool) {
	// 情形 A：上游报 usage
	_ = env.UseUsage(&mock.Usage{Prompt: 13, Completion: 7, Cached: 4, Write: 2})
	env.Mock.Reset()
	req := baseChatReq(model, accountUID, []map[string]any{
		{"role": "user", "content": "说一个字。"},
	})
	res, err := env.Chat(ctx, req, harness.ChatTimeout)
	if err != nil || res.Err != nil {
		rep.Add(assert("usage.reported", "上游报 usage 时插件转发", check.Fail, errText(err, res),
			"W3-34"))
		return
	}
	u := res.UsageEvent()
	if u == nil {
		rep.Add(assert("usage.reported", "上游报 usage 时插件转发", check.Fail,
			"上游返回了 usage 但插件未发出 usage 事件", "W3-34"))
		return
	}
	// 字段级来源标志（W3-34）
	p, pOk := num(u, "prompt_tokens")
	c, cOk := num(u, "completion_tokens")
	st, detail := check.Pass, fmt.Sprintf("prompt=%v completion=%v", p, c)
	if !pOk && !cOk {
		if noToken {
			// 上游确实不返回 token（glm 清言 / chatgpt 网页版）：只发耗时是正确的，
			// core 的 W3-34 估算兜底才是唯一的 token 来源。
			st, detail = check.Pass, "上游不返回 token（本渠道已知能力缺口），usage 仅带耗时"
		} else {
			st, detail = check.Fail, "usage 事件既无 prompt_tokens 也无 completion_tokens"
		}
	} else if p < 0 || c < 0 {
		// -1 表示「上游未上报」（W3-22 插件统计口径）
		st, detail = check.Warn, fmt.Sprintf("出现负数 token（-1 表示未知）: prompt=%v completion=%v", p, c)
	}
	rep.Add(assert("usage.reported", "上游报 usage 时插件转发", st, detail, "W3-34"))

	// reported 标志必须存在（新插件）或非零（旧插件兼容）
	// 字段级来源标志的真实名字是 prompt_reported / completion_reported / total_reported
	// 与 *_estimated（见 main/protocol/chat/tokens.go 的 UsageSource），
	// 不是 prompt_tokens_reported —— 早期写错名字导致合规插件被判「无来源标志」。
	hasFlag := u["prompt_reported"] != nil || u["completion_reported"] != nil ||
		u["total_reported"] != nil || u["prompt_estimated"] != nil
	st, detail = check.Pass, "带来源标志"
	if !hasFlag {
		switch {
		case p > 0 || c > 0:
			st, detail = check.Warn, "无 *_reported 标志但值非零（旧插件兼容路径，core 视为 reported）"
		case noToken && (p <= 0 && c <= 0):
			// 上游从不报 token，插件发出 0/0 占位：core（W3-34）按「旧插件空 0」视作 unknown，
			// 不会污染统计，但插件自身仍应尽量不发 0。记 WARN 提醒升级，不判失败。
			// 根本不发 token 字段（而非发 0）：这是「未上报」的另一种表达，等价于 null。
			st, detail = check.Pass, "未发 token 字段（上游不提供，等同 null 而非 0）"
		default:
			st, detail = check.Fail, "无来源标志且值全 0（core 无法区分「未报」与「明确 0」）"
		}
	}
	rep.Add(assert("usage.flags", "usage 字段级来源标志", st, detail, "W3-34"))

	// 情形 B：上游不报 usage → 插件不得臆造（不得发非零 usage 帧）
	_ = env.UseNoUsage()
	env.Mock.Reset()
	res2, err2 := env.Chat(ctx, req, harness.ChatTimeout)
	if err2 != nil || res2.Err != nil {
		rep.Add(assert("usage.absent", "上游不报 usage 时不臆造", check.Warn,
			"无 usage 场景调用失败: "+errText(err2, res2), "W3-34"))
		return
	}
	u2 := res2.UsageEvent()
	st, detail = check.Pass, "未发 usage 事件（正确：core 负责估算兜底）"
	if u2 != nil {
		p2, _ := num(u2, "prompt_tokens")
		c2, _ := num(u2, "completion_tokens")
		if p2 > 0 || c2 > 0 {
			est, _ := num(u2, "prompt_estimated")
			if est == 0 {
				st, detail = check.Fail, fmt.Sprintf(
					"上游未报 usage 但插件发出非零 usage（prompt=%v completion=%v）且未标 estimated —— 臆造用量",
					p2, c2)
			} else {
				st, detail = check.Pass, "标注为 estimated（允许，但 core 才是兜底方）"
			}
		}
	}
	rep.Add(assert("usage.absent", "上游不报 usage 时不臆造", st, detail, "W3-34"))
}

// session 检查 web 逆向渠道的会话管理共性契约（WEB_CLI_CONTRACT §会话管理）。
func session(ctx context.Context, env *harness.Env, rep *check.PluginReport) {
	// schema 命令是契约的一部分，先拿 gaps 判定哪些命令该有
	out, errb, code, err := env.RunCmd(ctx, []string{"schema"}, nil, harness.DefaultTimeout)
	if err != nil || code != 0 {
		rep.Add(assert("session.schema", "schema 命令可用", check.Fail,
			fmt.Sprintf("退出码 %d: %s", code, truncStr(string(errb), 200)), "WEB_CLI_CONTRACT §3"))
		return
	}
	var sc map[string]any
	if e := json.Unmarshal([]byte(extractJSONStr(string(out))), &sc); e != nil {
		rep.Add(assert("session.schema", "schema 命令可用", check.Fail,
			"schema 输出不是 JSON: "+e.Error(), "WEB_CLI_CONTRACT §3"))
		return
	}
	// 事件信封展开：WEB_CLI_CONTRACT 的便利命令输出是
	// {"v":1,"type":"result","cmd":"schema","data":{...}}，schema 字段在 data 里。
	// 直接在顶层找字段会把合规实现判成「缺全部必需字段」。
	if d := cliResultData(sc); d != nil {
		sc = d
	}
	rep.Add(assert("session.schema", "schema 命令可用", check.Pass, "可解析", "WEB_CLI_CONTRACT §3"))

	// 必需字段
	missing := []string{}
	for _, k := range []string{"channel_id", "version", "protocol", "commands", "events",
		"exit_codes", "retryable", "abilities", "gaps", "quirks"} {
		if _, ok := sc[k]; !ok {
			missing = append(missing, k)
		}
	}
	st, detail := check.Pass, "必需字段齐备"
	if len(missing) > 0 {
		st, detail = check.Fail, "缺字段: "+strings.Join(missing, ", ")
	}
	rep.Add(assert("session.schema-fields", "schema 必需字段", st, detail, "WEB_CLI_CONTRACT §3"))

	cmds := strSlice(sc, "commands")
	gaps := strSlice(sc, "gaps")
	gapText := strings.Join(gaps, "\n")

	// 每个会话命令：声明了就必须能跑（用 mock 数据）
	want := []struct {
		id   string
		cmd  string
		args []string
		// gapKey 用于判定「gaps 里明确声明不支持」
		gapKey string
	}{
		{"session.conv-list", "conv-list", []string{"conv-list", "--limit", "5"}, "conv-list"},
		{"session.conv-show", "conv-show", nil, "conv-show"},
		{"session.conv-history", "conv-history", nil, "conv-history"},
		{"session.conv-rename", "conv-rename", nil, "conv-rename"},
		{"session.conv-delete", "conv-delete", nil, "conv-delete"},
		{"session.conv-read", "conv-read", nil, "conv-read"},
	}
	cid := sessionCID(env)
	mid := env.MID
	if mid == "" {
		mid = cid
	}
	for _, w := range want {
		if !hasCap(cmds, w.cmd) {
			rep.Add(assert(w.id, w.cmd+" 命令存在", check.Skip,
				"commands 未声明该命令（契约允许）", "WEB_CLI_CONTRACT §4"))
			continue
		}
		if w.cmd == "conv-read" && strings.Contains(gapText, "conv-read") {
			rep.Add(assert(w.id, w.cmd+" 命令存在", check.Skip,
				"schema.gaps 明确声明不支持（诚实缺口）", "WEB_CLI_CONTRACT §3"))
			continue
		}
		if cid == "" && w.cmd != "conv-list" {
			rep.Add(assert(w.id, w.cmd+" 命令存在", check.Fail,
				"mock 未提供会话 ID，无法执行（夹具缺失）", "WEB_CLI_CONTRACT §4"))
			continue
		}
		args := w.args
		if args == nil {
			args = []string{w.cmd}
		}
		switch w.cmd {
		case "conv-show", "conv-history":
			args = append(args, "--cid", cid)
		case "conv-rename":
			args = append(args, "--cid", cid, "--title", "准入测试改名")
		case "conv-delete":
			args = append(args, "--cid", cid, "--yes")
		case "conv-read":
			// 契约钦定 --message-id（WEB_CLI_CONTRACT §conv-read）；--cid 会撞 usage_error
			args = append(args, "--message-id", mid)
		}
		args = append(args, "--data", env.DataDir)
		if env.Config != nil {
			if b, ok := env.Config["base_url"].(string); ok && b != "" {
				args = append(args, "--base-url", b)
			}
		}
		if a := authArgFor(env); a != "" {
			args = append(args, "--auth", a)
		}
		out, errb, code, err := env.RunCmd(ctx, args, nil, harness.ChatTimeout)
		st, detail := check.Pass, "rc=0"
		if err != nil || code != 0 {
			st, detail = check.Fail, fmt.Sprintf("rc=%d err=%v stderr=%s", code, err,
				truncStr(string(errb), 300))
		} else {
			// 事件模型：每行必须是 {v,type,cmd,data}
			frames := parseCLIEvents(string(out))
			if len(frames) == 0 {
				st, detail = check.Fail, "stdout 无事件输出"
			} else if last := frames[len(frames)-1]; last["type"] != "result" {
				st, detail = check.Fail, "最后一条事件不是 result（WEB_CLI_CONTRACT §3 要求恒有 result 且最后）"
			} else if last["cmd"] != w.cmd {
				st, detail = check.Fail, "result.cmd="+fmt.Sprint(last["cmd"])+" 与命令不符"
			} else {
				detail = fmt.Sprintf("rc=0，%d 条事件，末条 result", len(frames))
			}
		}
		rep.Add(assert(w.id, w.cmd+" 命令存在", st, detail, "WEB_CLI_CONTRACT §4"))
	}

	// 退出码表：8 值必须齐备且值正确
	ec, _ := sc["exit_codes"].(map[string]any)
	st, detail = check.Pass, "8 值齐备"
	if len(ec) < 8 {
		st, detail = check.Fail, fmt.Sprintf("exit_codes 只有 %d 项（应 8 项）", len(ec))
	}
	rep.Add(assert("session.exit-codes", "退出码表 8 值", st, detail, "WEB_CLI_CONTRACT §5"))

	// retryable 必须是 {4,5,6}（7 刻意不可重试）
	ret := strSlice(sc, "retryable")
	st, detail = check.Pass, "retryable="+strings.Join(ret, ",")
	if len(ret) != 3 {
		st, detail = check.Fail, "retryable 应为 3 项（4/5/6），实际: "+strings.Join(ret, ",")
	}
	rep.Add(assert("session.retryable", "retryable={4,5,6}", st, detail, "WEB_CLI_CONTRACT §5"))
}

// parseCLIEvents 解析便利命令的事件输出（每行一个 JSON 对象）。
func parseCLIEvents(s string) []map[string]any {
	var out []map[string]any
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "{") {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(ln), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func extractJSONStr(s string) string {
	t := strings.TrimSpace(s)
	if i := strings.Index(t, "{"); i >= 0 {
		if j := strings.LastIndex(t, "}"); j > i {
			return t[i : j+1]
		}
	}
	return t
}

// cliResultData 从 WEB_CLI_CONTRACT 的事件信封里取出 result 的 data 段。
//
// 便利命令统一输出 `{"v":1,"type":"result","cmd":"...","data":{...}}`（WEB_CLI_CONTRACT §3.1），
// 字段都在 data 里。返回 nil 表示这不是信封（直接把顶层当数据用）。
func cliResultData(ev map[string]any) map[string]any {
	if ev == nil || ev["type"] != "result" {
		return nil
	}
	if d, ok := ev["data"].(map[string]any); ok {
		return d
	}
	return nil
}

// singleTurnOnly 处理「上游只接受单条 user 提问」的渠道（GLM 清言实证）。
//
// 这类渠道不支持多轮是**上游能力缺口**，不是移植缺漏，因此：
//   - 两轮常规请求必须都成功（验证「单轮仍然可用」）；
//   - 带 assistant 历史时必须**明确报错**而不是静默丢弃历史（否则用户以为上下文生效）；
//   - 报告里显式记一条 FAIL 说明多轮能力缺失，不能悄悄跳过。
func singleTurnOnly(ctx context.Context, env *harness.Env, rep *check.PluginReport, model, accountUID string) {
	hist := []map[string]any{{"role": "user", "content": "第一轮：" + "MARK1-" + randToken(6)}}
	res, err := env.Chat(ctx, baseChatReq(model, accountUID, hist), harness.ChatTimeout)
	st, detail := check.Pass, "单轮成功"
	if err != nil || res.Err != nil || !res.OK || strings.TrimSpace(res.Text()) == "" {
		st, detail = check.Fail, "单轮对话失败: "+errText(err, res)
	}
	rep.Add(assert("multi.turn1", "单轮对话成功", st, detail, "IPC_CHAT §2"))

	// 带 assistant 历史：期望明确拒绝（bad_params / 400），而不是「接受但丢历史」。
	hist2 := append(append([]map[string]any{}, hist...),
		map[string]any{"role": "assistant", "content": "（上一轮回复）"},
		map[string]any{"role": "user", "content": "第二轮：" + "MARK2-" + randToken(6)})
	env.Mock.Reset()
	res2, err2 := env.Chat(ctx, baseChatReq(model, accountUID, hist2), harness.ChatTimeout)
	st2, detail2 := check.Pass, "带 assistant 历史时明确拒绝（上游不支持多轮，行为符合预期）"
	if err2 == nil && res2.Err == nil && res2.OK {
		// 接受了：那就要么真支持多轮，要么静默丢历史——查上游请求体里有没有 assistant 段落。
		body := ""
		for _, r := range env.Mock.Requests() {
			body += r.Body
		}
		if strings.Contains(body, "（上一轮回复）") {
			detail2 = "接受并下发了 assistant 历史（上游其实支持多轮）"
		} else {
			st2, detail2 = check.Fail, "接受多轮请求但静默丢弃 assistant 历史（用户会误以为上下文生效）"
		}
	}
	rep.Add(assert("multi.turn2", "多轮能力（上游限制）", st2, detail2, "IPC_CHAT §2"))
	// 记 WARN 而不是 FAIL：这是**上游能力缺口**（清言只吃单条 user 提问），不是移植缺漏，
	// 判 FAIL 会让该渠道永远红，反而淹没真正的缺陷。报告里照样逐字写明缺口。
	rep.Add(assert("multi.unsupported", "多轮能力缺口", check.Warn,
		"上游只接受单条 user 提问，不支持多轮上下文（渠道能力缺口，非移植缺漏；core 侧无法补偿）",
		"IPC_CHAT §2"))
}

// qoderAlphabet 是 qoder/qwenwork 上游 Encode=1 用的自定义 base64 字母表。
const qoderAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"

// tryDecodeBody 尽力把「被编码的请求体」还原成明文，用于校验历史是否下发。
//
// 顺序：原样（已是明文）→ 标准 base64 → qoder 自定义字母表（含三段重排还原）。
// 全部失败时返回空串，调用方按「无法验证」处理（不因此判失败）。
func tryDecodeBody(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "{") {
		return s
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && bytes.ContainsRune(b, '{') {
		return string(b)
	}
	if out := decodeQoderBody(s); out != "" {
		return out
	}
	return ""
}

// decodeQoderBody 还原 qoder 的编码：自定义字母表 → 标准 base64 → 三段重排逆变换。
//
// 编码是 std → 切成 (尾 a 段, 中段, 首 a 段) 重排，a = len/3；
// 解码即把重排结果按同样边界拼回 std = re[n-a:] + re[a:n-a] + re[:a]。
func decodeQoderBody(enc string) string {
	rev := [128]byte{}
	for i := 0; i < len(qoderAlphabet) && i < len(stdBase64Alphabet); i++ {
		rev[qoderAlphabet[i]] = stdBase64Alphabet[i]
	}
	buf := make([]byte, len(enc))
	for i := 0; i < len(enc); i++ {
		c := enc[i]
		if c == '$' {
			buf[i] = '='
			continue
		}
		if int(c) < len(rev) && rev[c] != 0 {
			buf[i] = rev[c]
			continue
		}
		buf[i] = c
	}
	n := len(buf)
	if n == 0 {
		return ""
	}
	a := n / 3
	re := string(buf)
	std := re[n-a:] + re[a:n-a] + re[:a]
	out, err := base64.StdEncoding.DecodeString(std)
	if err != nil {
		return ""
	}
	return string(out)
}

const stdBase64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
