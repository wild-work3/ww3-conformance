package suite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wild-work3/ww3-conformance/internal/check"
	"github.com/wild-work3/ww3-conformance/internal/harness"
)

// live 检查真实上游层：用有效凭据 + flash 类低消耗模型做**一轮**真实抽查。
//
// 用户 m01897 要求：无有效凭据、上游对抗规则变更导致无法实测，必须在报告里
// 明确报告为 FAIL —— 不得静默跳过。
// 用户后续追加：真实上游**只跑一轮**（默认 LiveTurns=1），避免触发风控。
func live(ctx context.Context, env *harness.Env, rep *check.PluginReport, d Descriptor, turns int) {
	// 1) 凭据：匿名渠道无需凭据；其余必须有可读的凭据文件
	name := ""
	switch {
	case d.LiveAuthFile != "":
		authPath := absPath(d.LiveAuthFile)
		if !exists(authPath) {
			// 用户 m01897 要求：没有有效凭据必须显式报告失败，不得静默跳过。
			rep.Add(assert("live.auth", "存在有效真实凭据", check.Fail,
				"无真实凭据可用（"+authPath+" 不存在）：离线契约已验，**真实上游未验证**",
				"用户 m01897 要求"))
			rep.ModeReason = "无真实凭据"
			return
		}
		raw, err := os.ReadFile(authPath)
		if err != nil {
			rep.Add(assert("live.auth", "存在有效真实凭据", check.Fail, "读凭据失败: "+err.Error(),
				"用户 m01897 要求"))
			return
		}
		// 复制到夹具数据目录，避免污染原始凭据（不回写：上游轮换 token 时原目录不更新）
		name = strings.TrimSuffix(filepath.Base(authPath), ".json")
		if err := env.WriteAuthRaw(name, raw); err != nil {
			rep.Add(assert("live.auth", "存在有效真实凭据", check.Fail, "复制凭据失败: "+err.Error(),
				"用户 m01897 要求"))
			return
		}
		rep.Add(assert("live.auth", "存在有效真实凭据", check.Pass, "已加载 "+name+"（复制到夹具，不回写）",
			"用户 m01897 要求"))
	case d.LiveAnon:
		rep.Add(assert("live.auth", "凭据要求", check.Pass, "匿名通道，无需凭据（未下发 --auth）",
			"用户 m01897 要求"))
	default:
		rep.Add(assert("live.auth", "存在有效真实凭据", check.Fail,
			"无真实凭据可用（未提供 --auth/--data）：离线契约已验，**真实上游未验证**",
			"用户 m01897 要求"))
		rep.ModeReason = "无真实凭据"
		return
	}

	// 2) 模型：必须是 flash 类低消耗模型
	// LiveModel 写成 "auto" 则先向真实上游取一次模型目录（元数据请求，风险低于对话），
	// 再挑 flash/mini/lite 类模型 —— 避免因为描述符里的模型名在该区已下线而误判
	// 「渠道已死」。
	model := d.LiveModel
	if model == "" || strings.EqualFold(model, "auto") {
		picked, detail, err := pickLiveModel(ctx, env, d, name)
		if err != nil {
			rep.Add(assert("live.model", "指定 flash 类低消耗模型", check.Fail,
				"真实模型目录拉取失败: "+err.Error()+" —— 上游可能已变更或凭据失效", "用户 m01897 要求"))
			rep.ModeReason = "真实模型目录不可用"
			return
		}
		model = picked
		rep.Add(assert("live.model", "指定 flash 类低消耗模型", check.Pass, model+"（来自真实目录 "+detail+"）",
			"用户 m01897 要求"))
	} else {
		rep.Add(assert("live.model", "指定 flash 类低消耗模型", check.Pass, model, "用户 m01897 要求"))
	}

	// 3) 真实单轮：不走 mock（base_url 置空，用官方地址）
	// base_url 必须**显式置空**而不是「省略」：Env.Config 是离线夹具的渠道配置，
	// 省略时 harness 仍会把整份 Config 当 channel_config 下发（见 Env.Chat），
	// mock 地址就又生效了 —— 实测 live 用例因此打到了本地 mock 上。
	lcfg := liveCfg(env)
	// 真实上游用短 prompt + 小 max_tokens 控制消耗
	req := map[string]any{
		"model":       model,
		"account_uid": name,
		"stream":      true,
		"max_tokens":  64,
		"messages": []any{
			map[string]any{"role": "user", "content": "只回复两个字：收到"},
		},
	}
	req["channel_config"] = lcfg
	start := time.Now()
	res, err := env.Chat(ctx, req, harness.ChatTimeout)
	took := time.Since(start)

	if err != nil {
		rep.Add(assert("live.chat", "真实上游单轮对话", check.Fail,
			fmt.Sprintf("调用失败（耗时 %s）: %v", took.Round(time.Millisecond), err),
			"用户 m01897 要求"))
		return
	}
	if res.Err != nil {
		// 分类失败原因：凭据失效 / 上游对抗规则变更 / 其他
		kind := res.Err.Kind
		hint := "上游可能已修改对抗规则或凭据已失效，需人工复核"
		switch kind {
		case "auth", "session_dead", "account_fault":
			hint = "凭据已失效（需重新登录）"
		case "waf_block", "content_blocked", "not_proven":
			hint = "上游对抗规则已变更（签名/指纹/风控被拒）"
		case "not_found", "model_blocked":
			hint = "模型不可用或已下线"
		case "soft_rate", "hard_credit", "queued":
			hint = "上游限流/额度不足"
		}
		rep.Add(assert("live.chat", "真实上游单轮对话", check.Fail,
			fmt.Sprintf("[%s] %s —— %s", kind, res.Err.Message, hint), "用户 m01897 要求"))
		rep.ModeReason = "真实上游失败: " + kind
		return
	}
	if !res.OK {
		rep.Add(assert("live.chat", "真实上游单轮对话", check.Fail, "ok=false", "用户 m01897 要求"))
		return
	}
	text := strings.TrimSpace(res.Text())
	st, detail := check.Pass, fmt.Sprintf("%d 字，耗时 %s", len([]rune(text)), took.Round(time.Millisecond))
	if text == "" {
		st, detail = check.Fail, "正文为空（上游返回空流，可能对抗规则已变更）"
	}
	rep.Add(assert("live.chat", "真实上游单轮对话", st, detail, "用户 m01897 要求"))

	// 4) 真实多轮（可选）：默认不跑 —— 用户要求真实上游只跑一轮，避免触发风控。
	if turns < 2 {
		rep.Add(assert("live.multi", "真实上游多轮对话", check.Warn,
			"按用户要求只跑 1 轮真实请求（避免触发风控），真实上游的多轮上下文**未验证**；"+
				"需要时用 --live-turns 2 开启",
			"用户要求（风控优先）"))
		return
	}

	// 两条合法路径都要覆盖：①重放型渠道下发完整历史；②会话式渠道（chatgpt 类 web
	// 逆向只取最后一个 user 提问）靠 conversation_id 续接 —— 首轮返回的会话 ID
	// 必须回传，否则第二轮就是全新会话，历史真的丢了。
	req2 := map[string]any{
		"model":       model,
		"account_uid": name,
		"stream":      true,
		"max_tokens":  64,
		"messages": []any{
			map[string]any{"role": "user", "content": "记住数字 7。"},
			map[string]any{"role": "assistant", "content": text},
			map[string]any{"role": "user", "content": "我刚才让你记的数字是几？只回数字。"},
		},
	}
	if cid := str(res.Obj, "conversation_id"); cid != "" {
		req2["conversation_id"] = cid
	}
	req2["channel_config"] = lcfg
	res2, err2 := env.Chat(ctx, req2, harness.ChatTimeout)
	if err2 != nil || res2.Err != nil || !res2.OK {
		rep.Add(assert("live.multi", "真实上游多轮对话", check.Fail, errText(err2, res2),
			"用户 m01897 要求"))
		return
	}
	// 会话连续性：会话式渠道必须复用同一个会话 ID（否则「多轮」只是把历史塞进
	// 一次请求，续接点全丢）。重放型渠道不返回 conversation_id，此项不适用。
	if cid1 := str(res.Obj, "conversation_id"); cid1 != "" {
		cid2 := str(res2.Obj, "conversation_id")
		st, detail := check.Pass, "会话 ID 连续: "+cid1
		if cid2 == "" {
			st, detail = check.Warn, "首轮有 conversation_id 但第二轮未回传（插件未透出会话 ID）"
		} else if cid2 != cid1 {
			st, detail = check.Fail, "两轮落在不同会话: "+cid1+" → "+cid2+"（上下文未续接）"
		}
		rep.Add(assert("live.session", "真实上游多轮会话连续", st, detail, "W3-11"))
	}

	t2 := strings.TrimSpace(res2.Text())
	st, detail = check.Pass, fmt.Sprintf("%d 字", len([]rune(t2)))
	if t2 == "" {
		st, detail = check.Fail, "第二轮正文为空"
	} else if !strings.Contains(t2, "7") {
		// 不是硬失败：模型可能不听话；但历史是否下发由离线用例保证
		st, detail = check.Warn, "第二轮未复述数字 7（模型行为，非契约缺陷）: "+truncStr(t2, 80)
	}
	rep.Add(assert("live.multi", "真实上游多轮对话", st, detail, "用户 m01897 要求"))
}

// liveCfg 构造真实上游的渠道配置：沿用描述符的 ExtraConfig（region/base_url 等），
// 但把 base_url 清空指向官方地址，并在需要时注入代理。
//
// base_url 必须显式清空而不是删除键：Env.Config 是 mock 夹具配置，留着它会把
// 请求打到本地 mock 上（live 因此出现过「真实上游通过」的假结论）。
func liveCfg(env *harness.Env) map[string]any {
	out := map[string]any{}
	for k, v := range env.Config {
		if k == "base_url" {
			continue
		}
		out[k] = v
	}
	out["base_url"] = ""
	if px := strings.TrimSpace(os.Getenv("WW3_LIVE_PROXY")); px != "" {
		out["proxy"] = px
	}
	return out
}

// pickLiveModel 向真实上游取模型目录并挑一个 flash 类低消耗模型。
// 返回 (模型ID, 目录来源描述, error)。目录拉不到即说明凭据/对抗规则有问题，
// 直接判失败（用户要求这类情况必须显式失败）。
func pickLiveModel(ctx context.Context, env *harness.Env, d Descriptor, name string) (string, string, error) {
	p := map[string]any{"uid": name, "config": liveCfg(env)}
	res, err := env.Call(ctx, "models.list", p, harness.DefaultTimeout)
	if err != nil {
		return "", "", err
	}
	if res.Err != nil {
		return "", "", fmt.Errorf("[%s] %s", res.Err.Kind, res.Err.Message)
	}
	ms, _ := res.Obj["models"].([]any)
	ids := make([]string, 0, len(ms))
	for _, it := range ms {
		if m, ok := it.(map[string]any); ok {
			if id, _ := m["id"].(string); id != "" {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return "", "", fmt.Errorf("真实模型目录为空")
	}
	pick := pickFlashModel(ids)
	return pick, fmt.Sprintf("%d 个模型", len(ids)), nil
}

// pickFlashModel 按「便宜/快」的偏好挑一个模型：flash > mini > lite > free > turbo > air，
// 都不命中则取第一个（仍只跑 1 轮、max_tokens=64，消耗可控）。
func pickFlashModel(ids []string) string {
	for _, kw := range []string{"flash", "mini", "lite", "free", "turbo", "air"} {
		for _, id := range ids {
			if strings.Contains(strings.ToLower(id), kw) {
				return id
			}
		}
	}
	return ids[0]
}
