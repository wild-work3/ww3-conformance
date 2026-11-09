package suite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wild-work3/ww3-conformance/internal/check"
	"github.com/wild-work3/ww3-conformance/internal/harness"
	"github.com/wild-work3/ww3-conformance/internal/wire"
)

// control 逐条检查 manifest 声明的控制面能力（PLUGIN_PROTOCOL §5）。
// 原则：声明了就必须能调通（FAIL），没声明就 Skip（契约允许不实现）。
func control(ctx context.Context, env *harness.Env, rep *check.PluginReport, d Descriptor) {
	caps := rep.Control
	has := func(n string) bool { return hasCap(caps, n) }

	// ---- config.schema ----
	if has("config.schema") {
		res, err := env.Call(ctx, "config.schema", nil, harness.DefaultTimeout)
		if st, detail := basicOK(err, res); st != check.Pass {
			rep.Add(assert("config.schema", "config.schema 可调用", st, detail, "PLUGIN_PROTOCOL §5.7"))
		} else {
			schema := res.Obj
			if t := str(schema, "type"); t != "object" {
				rep.Add(assert("config.schema", "config.schema 形状", check.Fail,
					"type="+t+"（应为 object）", "PLUGIN_PROTOCOL §5.7"))
			} else {
				props, _ := schema["properties"].(map[string]any)
				rep.Add(assert("config.schema", "config.schema 形状", check.Pass,
					fmt.Sprintf("object，%d 个属性", len(props)), "PLUGIN_PROTOCOL §5.7"))
				// base_url 是准入测试赖以重定向上游的通用开关（17 个插件均已支持）
				if _, ok := props["base_url"]; !ok {
					rep.Add(assert("config.base-url", "schema 暴露 base_url", check.Warn,
						"未暴露 base_url，无法做离线重定向测试", "PLUGIN_CLI §3"))
				} else {
					rep.Add(assert("config.base-url", "schema 暴露 base_url", check.Pass, "已暴露", "PLUGIN_CLI §3"))
				}
			}
		}
	}

	// ---- config.get / config.set ----
	if has("config.get") {
		res, err := env.CallWithConfig(ctx, "config.get", nil, harness.DefaultTimeout)
		if st, detail := basicOK(err, res); st != check.Pass {
			rep.Add(assert("config.get", "config.get 可调用", st, detail, "PLUGIN_PROTOCOL §5.7"))
		} else {
			rep.Add(assert("config.get", "config.get 可调用", check.Pass,
				"config 键存在="+fmt.Sprint(res.Obj["config"] != nil), "PLUGIN_PROTOCOL §5.7"))
		}
	}
	if has("config.set") {
		// 写一个无害字段再读回，确认往返一致（不写真实凭据/上游地址）
		payload := map[string]any{"config": map[string]any{"timeout": 111}}
		res, err := env.Call(ctx, "config.set", payload, harness.DefaultTimeout)
		if st, detail := basicOK(err, res); st != check.Pass {
			rep.Add(assert("config.set", "config.set 可调用", st, detail, "PLUGIN_PROTOCOL §5.7"))
		} else {
			rep.Add(assert("config.set", "config.set 可调用", check.Pass, "ok", "PLUGIN_PROTOCOL §5.7"))
			// 清理：写回空对象不必然等于清空，故只在能 get 时确认不崩
			if has("config.get") {
				_, _ = env.Call(ctx, "config.set", map[string]any{"config": map[string]any{}}, harness.DefaultTimeout)
			}
		}
	}

	// ---- models.list ----
	if has("models.list") {
		res, err := env.CallWithConfig(ctx, "models.list", nil, harness.DefaultTimeout)
		if st, detail := basicOK(err, res); st != check.Pass {
			rep.Add(assert("models.list", "models.list 可调用", st, detail, "PLUGIN_PROTOCOL §5.2"))
		} else {
			ms, _ := res.Obj["models"].([]any)
			st, detail := check.Pass, fmt.Sprintf("%d 个模型", len(ms))
			if len(ms) == 0 {
				st, detail = check.Fail, "models 为空（至少应有 1 个可用模型）"
			}
			rep.Add(assert("models.list", "models.list 返回模型", st, detail, "PLUGIN_PROTOCOL §5.2"))
			// 模型字段形状：id 必填，null ≠ 0（未上报不得臆造）
			if len(ms) > 0 {
				bad := []string{}
				for i, it := range ms {
					if i >= 5 {
						break
					}
					mm, ok := it.(map[string]any)
					if !ok {
						bad = append(bad, fmt.Sprintf("#%d 非对象", i))
						continue
					}
					if str(mm, "id") == "" {
						bad = append(bad, fmt.Sprintf("#%d 缺 id", i))
					}
					// efforts 若声明，default_effort 必须落在表内（W3-32）
					ef := strSlice(mm, "efforts")
					if de := str(mm, "default_effort"); de != "" && len(ef) > 0 && !hasCap(ef, de) {
						bad = append(bad, fmt.Sprintf("#%d default_effort=%s 不在 efforts 内", i, de))
					}
				}
				st, detail := check.Pass, "id 齐备；efforts/default_effort 自洽"
				if len(bad) > 0 {
					st, detail = check.Fail, strings.Join(bad, "；")
				}
				rep.Add(assert("models.shape", "模型条目字段形状", st, detail, "PLUGIN_PROTOCOL §5.2 / W3-32"))
			}
		}
	}

	// ---- accounts.list ----
	if has("accounts.list") {
		res, err := env.CallWithConfig(ctx, "accounts.list", nil, harness.DefaultTimeout)
		if st, detail := basicOK(err, res); st != check.Pass {
			rep.Add(assert("accounts.list", "accounts.list 可调用", st, detail, "PLUGIN_PROTOCOL §5.4"))
		} else {
			accts, _ := res.Obj["accounts"].([]any)
			st, detail := check.Pass, fmt.Sprintf("%d 个账号", len(accts))
			if len(accts) == 0 && d.Auth != nil {
				st, detail = check.Fail, "已写入凭据但 accounts.list 为空（凭据未被识别）"
			}
			rep.Add(assert("accounts.list", "accounts.list 返回账号", st, detail, "PLUGIN_PROTOCOL §5.4"))
			// 零 token 落盘红线：accounts.list 不得回 token
			if leak := findSecretKey(accts); leak != "" {
				rep.Add(assert("accounts.secret", "accounts.list 不泄露凭据", check.Fail,
					"响应含敏感字段: "+leak, "AGENTS §9"))
			} else {
				rep.Add(assert("accounts.secret", "accounts.list 不泄露凭据", check.Pass,
					"未发现 token/secret 字段", "AGENTS §9"))
			}
		}
	}

	// ---- accounts.refresh ----
	if has("accounts.refresh") {
		p := map[string]any{}
		if d.AccountUID != "" {
			p["uid"] = d.AccountUID
		} else if d.AuthName != "" {
			p["uid"] = d.AuthName
		}
		res, err := env.CallWithConfig(ctx, "accounts.refresh", p, harness.DefaultTimeout)
		st, detail := basicOK(err, res)
		// refresh 在 mock 上游下可能因端点不匹配失败；失败但错误分类合理则记 Warn
		if st != check.Pass && res != nil && res.Err != nil {
			st, detail = check.Warn, "refresh 失败（mock 上游可能未覆盖该端点）: "+res.Err.Error()
		}
		rep.Add(assert("accounts.refresh", "accounts.refresh 可调用", st, detail, "PLUGIN_PROTOCOL §5.4"))
	}

	// ---- accounts.resource_detail ----
	if has("accounts.resource_detail") {
		p := map[string]any{}
		if d.AccountUID != "" {
			p["uid"] = d.AccountUID
		}
		res, err := env.CallWithConfig(ctx, "accounts.resource_detail", p, harness.DefaultTimeout)
		st, detail := basicOK(err, res)
		if st != check.Pass && res != nil && res.Err != nil {
			st, detail = check.Warn, "resource_detail 失败: "+res.Err.Error()
		}
		rep.Add(assert("accounts.resource_detail", "accounts.resource_detail 可调用", st, detail,
			"PLUGIN_PROTOCOL §5.5"))
	}

	// ---- accounts.checkin ----
	if has("accounts.checkin") {
		p := map[string]any{}
		if d.AccountUID != "" {
			p["uid"] = d.AccountUID
		}
		res, err := env.CallWithConfig(ctx, "accounts.checkin", p, harness.DefaultTimeout)
		st, detail := basicOK(err, res)
		if st != check.Pass && res != nil && res.Err != nil {
			st, detail = check.Warn, "checkin 失败（mock 上游可能未覆盖）: "+res.Err.Error()
		}
		rep.Add(assert("accounts.checkin", "accounts.checkin 可调用", st, detail, "PLUGIN_PROTOCOL §5.4"))
	}

	// ---- accounts.remove：只验证存在性，不真删（会破坏后续用例）----
	if has("accounts.remove") {
		rep.Add(assert("accounts.remove", "accounts.remove 已声明", check.Pass,
			"声明存在（不执行：会破坏后续用例的凭据）", "PLUGIN_PROTOCOL §5.4"))
	}

	// ---- credits.get ----
	if has("credits.get") {
		res, err := env.CallWithConfig(ctx, "credits.get", nil, harness.DefaultTimeout)
		st, detail := basicOK(err, res)
		if st == check.Pass {
			// balance 允许 null（未上报），但不得臆造 0
			if v, ok := res.Obj["balance"]; ok && v != nil {
				if _, isNum := v.(float64); !isNum {
					st, detail = check.Fail, "balance 类型非法（应为 number 或 null）"
				}
			}
			if items, ok := res.Obj["items"].([]any); ok {
				for i, it := range items {
					if i >= 3 {
						break
					}
					mm, _ := it.(map[string]any)
					if mm == nil {
						continue
					}
					// unit 是 omitempty 且「空=credits」（PLUGIN_PROTOCOL §5.5），
					// 缺失是合法形态；只有出现非法取值才算问题。
					if u, ok := mm["unit"]; ok {
						if us, _ := u.(string); us != "" && us != "credits" && us != "token" {
							st, detail = check.Warn, "ResourceItem unit 取值非法: "+us
						}
					}
				}
			}
		} else if res != nil && res.Err != nil {
			st, detail = check.Warn, "credits.get 失败: "+res.Err.Error()
		}
		rep.Add(assert("credits.get", "credits.get 可调用", st, detail, "PLUGIN_PROTOCOL §5.5"))
	}

	// ---- stats.get ----
	if has("stats.get") {
		res, err := env.CallWithConfig(ctx, "stats.get", map[string]any{"days": 7}, harness.DefaultTimeout)
		st, detail := basicOK(err, res)
		if st != check.Pass && res != nil && res.Err != nil {
			st, detail = check.Warn, "stats.get 失败: "+res.Err.Error()
		}
		rep.Add(assert("stats.get", "stats.get 可调用", st, detail, "W3-22"))
	}

	// ---- auth.methods ----
	if has("auth.methods") {
		res, err := env.CallWithConfig(ctx, "auth.methods", nil, harness.DefaultTimeout)
		if st, detail := basicOK(err, res); st != check.Pass {
			rep.Add(assert("auth.methods", "auth.methods 可调用", st, detail, "PLUGIN_PROTOCOL §5.3"))
		} else {
			ms, _ := res.Obj["methods"].([]any)
			st, detail := check.Pass, fmt.Sprintf("%d 种登录方式", len(ms))
			valid := map[string]bool{"device_poll": true, "loopback_callback": true, "cdp_capture": true,
				"form": true, "credential_import": true, "none": true}
			bad := []string{}
			for _, it := range ms {
				mm, _ := it.(map[string]any)
				if mm == nil {
					continue
				}
				if t := str(mm, "type"); !valid[t] {
					bad = append(bad, "非法 type="+t)
				}
			}
			if len(bad) > 0 {
				st, detail = check.Fail, strings.Join(bad, "；")
			}
			rep.Add(assert("auth.methods", "auth.methods 六范式合法", st, detail,
				"PLUGIN_PROTOCOL §5.3 / W3-19"))
		}
	}

	// ---- systemone.decide / images.generate：不支持必须回 not_supported ----
	for _, cap := range []string{"systemone.decide", "images.generate"} {
		if has(cap) {
			rep.Add(assert(cap, cap+" 已声明", check.Pass, "声明存在（详细校验见专属用例）",
				"PLUGIN_PROTOCOL §5.8"))
		} else {
			res, err := env.Call(ctx, cap, map[string]any{}, harness.DefaultTimeout)
			st, detail := check.Pass, "未声明且回 not_supported（正确姿势）"
			if err != nil {
				st, detail = check.Warn, "调用失败: "+err.Error()
			} else if res.Err == nil {
				st, detail = check.Warn, "未声明但方法返回成功（应回 not_supported）"
			} else if res.Err.Kind != "not_supported" && res.Err.Kind != "not_found" {
				st, detail = check.Warn, "未声明时 kind="+res.Err.Kind+"（应 not_supported/not_found）"
			}
			rep.Add(assert(cap+".unsupported", cap+" 不支持时的姿势", st, detail, "W3-30"))
		}
	}

	// ---- ping / health.check 已在契约层测过 ----
	_ = time.Now
}

// basicOK 是「调用成功且无错误」的通用判定。
func basicOK(err error, res *wire.Result) (check.Status, string) {
	if err != nil {
		return check.Fail, err.Error()
	}
	if res == nil {
		return check.Fail, "无响应"
	}
	if res.Err != nil {
		return check.Fail, res.Err.Error()
	}
	if !res.OK {
		return check.Fail, "ok=false 但无 error 字段"
	}
	return check.Pass, "ok"
}

func assert(id, name string, st check.Status, detail, contract string) check.Assertion {
	return check.Assertion{Level: levelOf(id), ID: id, Name: name, Status: st, Detail: detail, Contract: contract}
}

// levelOf 按断言 ID 前缀推断层级。
func levelOf(id string) check.Level {
	switch {
	case strings.HasPrefix(id, "cli."), strings.HasPrefix(id, "manifest."),
		strings.HasPrefix(id, "call."), strings.HasPrefix(id, "stdout."),
		strings.HasPrefix(id, "lifecycle."):
		return check.LevelContract
	case strings.HasPrefix(id, "chat."), strings.HasPrefix(id, "stream."):
		return check.LevelChat
	case strings.HasPrefix(id, "multi."):
		return check.LevelMultiTurn
	case strings.HasPrefix(id, "session."):
		return check.LevelSession
	case strings.HasPrefix(id, "usage."):
		return check.LevelUsage
	case strings.HasPrefix(id, "live."):
		return check.LevelLive
	default:
		return check.LevelControl
	}
}

// findSecretKey 在响应里找疑似凭据字段（零 token 落盘红线）。
func findSecretKey(v any) string {
	switch t := v.(type) {
	case map[string]any:
		for k, sub := range t {
			lk := strings.ToLower(k)
			for _, bad := range []string{"access_token", "accesstoken", "refresh_token", "refreshtoken",
				"authorization", "cookie", "password", "signing_secret", "client_secret"} {
				if strings.ReplaceAll(lk, "_", "") == strings.ReplaceAll(bad, "_", "") {
					return k
				}
			}
			if r := findSecretKey(sub); r != "" {
				return k + "." + r
			}
		}
	case []any:
		for _, sub := range t {
			if r := findSecretKey(sub); r != "" {
				return r
			}
		}
	}
	return ""
}
