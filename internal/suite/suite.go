// Package suite 是渠道插件准入测试套件：按 protocol/docs/PLUGIN_PROTOCOL.md、
// protocol/docs/PLUGIN_CLI.md、protocol/docs/IPC_CHAT.md 的文本契约，对任意渠道插件二进制做标准化检查。
//
// 分层（与 check.Level 对应）：
//
//	契约骨架   manifest / ping / initialize / capabilities.list / stdout 纯 JSONL / 未知方法
//	控制面     逐条声明的 control 能力冒烟（models.list / config.* / accounts.* / auth.* / credits.get …）
//	会话面     chat.generate 流式事件序列、结束语义、错误分类
//	多轮对话   三轮真实场景，验证上下文累积与消息历史完整下发
//	会话管理   web 逆向渠道的 conv-list/show/history/rename/delete（按 schema.gaps 判定）
//	用量口径   usage 字段归一（reported vs unknown，禁止臆造）
//	真实上游   flash 类低消耗模型抽查（需有效凭据，缺失即 FAIL）
//
// 判定原则（用户 m01897 明确要求）：
//   - SKIP 只用于「契约允许不实现」的情形：能力未声明、schema.gaps 明确声明缺口。
//   - 无有效凭据、上游对抗规则变更、真实上游不可达 → 一律 FAIL，不得静默跳过。
package suite

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wild-work3/ww3-conformance/internal/check"
	"github.com/wild-work3/ww3-conformance/internal/harness"
	"github.com/wild-work3/ww3-conformance/internal/mock"
	"github.com/wild-work3/ww3-conformance/internal/wire"
)

// Descriptor 描述一个插件的准入测试夹具。
//
// 它同时是 `conformance.json`（B 层描述文件）的内存形态：字段一一对应，
// 只是 Go 侧的 Routes 是 []mock.Route，JSON 侧是数组。
type Descriptor struct {
	// ID 是插件 ID（manifest.id 应一致）。
	ID string
	// Kind 是插件类型：channel | hook。空视同 channel（旧预设都不带）。
	Kind string
	// Exe 是可执行文件路径（可以是解释器，如 python.exe）。
	Exe string
	// Args 是透传给被测进程的前缀参数（`--` 之后的参数），单 exe 插件为空。
	Args []string
	// Model 是离线测试用的模型名（带渠道前缀）。空则取 models.list 第一条。
	Model string
	// LiveModel 是真实上游抽查用的 flash 类模型名。空表示该渠道无 live 用例。
	LiveModel string
	// Auth 是离线凭据（nil 表示该渠道无需凭据，如匿名渠道）。
	Auth    map[string]any
	Account map[string]any
	// AuthName 是凭据文件名（同时是 account_uid）。空则用 ID+"-mock"。
	AuthName string
	// AccountUID 是 chat.generate 下发的 account_uid。空则用 AuthName。
	AccountUID string
	// ExtraConfig 是额外渠道配置（与 base_url 合并）。
	ExtraConfig map[string]any
	// Routes 是 mock 上游路由表。
	Routes []mock.Route
	// RoutesFor 允许按 base_url 动态生成路由（用于需要知道 mock 地址的场景）。
	RoutesFor func(base string) []mock.Route
	// SingleTurn 声明上游只接受「单条 user 提问」（GLM 清言实证：assistant 段落
	// 直接 400）。此时多轮用例无法按常规验证，报告里必须显式写出这个能力缺口。
	SingleTurn bool
	// NoTokenUsage 声明上游根本不返回 token 用量（glm / chatgpt 网页版）。
	// 此时 usage 事件只带耗时是**正确行为**，不能按「缺 prompt_tokens」判失败；
	// 但仍必须验证「没有臆造非零 token」。
	NoTokenUsage bool
	// Session 声明该插件是否支持会话管理命令（conv-*）。
	Session bool
	// SessionCID 是会话管理用例要用的会话 ID（mock 上游固定返回同一个）。
	SessionCID string
	// SessionMID 是按消息操作的会话命令（conv-read）要用的消息 ID（24hex）。
	// 空则回退 SessionCID。
	SessionMID string
	// LiveAuthFile 是真实凭据文件路径（相对 conformance 仓或绝对路径）。
	LiveAuthFile string
	// AuthPrefixes 是真实凭据文件名前缀（自动发现用），如 traework 的 "trae"、
	// qoder 的 "qodercn"/"qodercom"、codearts 的 "comate"。空则回退 ID。
	AuthPrefixes []string
	// LiveAnon 声明该渠道无需凭据即可真实调用（匿名通道，如 kilo/oczen）。
	LiveAnon bool
	// Notes 是给报告的备注。
	Notes []string
	// Hook 是拦截器插件专属的可选配置（只有 kind=hook 用得上）。
	Hook *hookConfig
}

// Options 是运行参数。
type Options struct {
	// Mode 决定是否跑 live 层。
	Mode check.Mode
	// Keep 保留临时目录（排查用）。
	Keep bool
	// WorkDir 是临时根目录（空则用 os.MkdirTemp）。
	WorkDir string
	// Verbose 打更多日志到 stderr。
	Verbose bool
	// LiveTurns 是真实上游对话轮数：1=只跑一轮（默认，避免触发风控），2=再跑一轮验上下文。
	LiveTurns int
}

// Run 对一个插件跑全部适用层级的检查。
func Run(ctx context.Context, d Descriptor, opts Options) *check.PluginReport {
	rep := &check.PluginReport{
		PluginID: d.ID, Exe: d.Exe, Mode: opts.Mode,
		Started: time.Now(), Notes: append([]string{}, d.Notes...),
	}
	root := opts.WorkDir
	if root == "" {
		var err error
		root, err = os.MkdirTemp("", "ww3-conf-"+d.ID+"-")
		if err != nil {
			rep.Block("无法创建临时目录: " + err.Error())
			rep.Ended = time.Now()
			return rep
		}
	}
	if !opts.Keep {
		defer os.RemoveAll(root)
	} else {
		rep.Notes = append(rep.Notes, "临时目录保留在 "+root)
	}

	env, err := harness.NewEnv(root, d.ID, d.Exe, d.Args)
	if err != nil {
		rep.Block("无法创建夹具: " + err.Error())
		rep.Ended = time.Now()
		return rep
	}
	defer env.Close()

	// ---- mock 上游 + 凭据夹具（必须早于契约骨架）----
	//
	// 契约骨架里的 health.check 会读插件本地状态（如 cline 的 ready = 有凭据 || 配了 Key）。
	// 夹具晚写会让它自报 unconfigured —— 那是「没喂夹具」而不是契约违背，误报过一次。
	hasMock := len(d.Routes) > 0 || d.RoutesFor != nil
	if hasMock {
		if err := setupMock(env, d); err != nil {
			rep.Add(check.Assertion{Level: check.LevelContract, ID: "mock.setup", Name: "mock 上游可启动",
				Status: check.Fail, Detail: err.Error()})
			rep.Blocked = "mock 上游启动失败"
			rep.Ended = time.Now()
			return rep
		}
	}
	if d.Auth != nil {
		name := d.AuthName
		if name == "" {
			name = d.ID + "-mock"
		}
		if err := env.WriteAuth(name, d.Auth, d.Account); err != nil {
			rep.Add(check.Assertion{Level: check.LevelContract, ID: "auth.fixture", Name: "凭据夹具可写入",
				Status: check.Fail, Detail: err.Error()})
		}
	}

	// ---- 契约骨架 ----
	m := contract(ctx, env, rep)
	if m == nil {
		// manifest 拿不到，后续全部无法进行
		rep.Blocked = "manifest 不可用，无法继续"
		rep.Ended = time.Now()
		return rep
	}
	rep.PluginID = strOr(m, d.ID, "id")
	rep.Name = str(m, "name")
	rep.Version = str(m, "version")
	rep.Kind = str(m, "kind")
	rep.Control = strSlice(m, "capabilities", "control")
	rep.Conversation = strSlice(m, "capabilities", "conversation")

	accountUID := d.AccountUID
	if accountUID == "" {
		accountUID = d.AuthName
		if accountUID == "" && d.Auth != nil {
			accountUID = d.ID + "-mock"
		}
	}
	env.AuthName = accountUID
	env.CID = d.SessionCID
	env.MID = d.SessionMID

	// ---- 控制面 ----
	//
	// A 层（只给 --exe、没有描述文件）的硬边界：**绝不静默打真上游**。
	// 无 mock 时 env.Config 里没有 base_url，models.list/credits.get 这类控制面调用
	// 会走插件自己的默认端点——那是真实上游（AGENTS §6 严禁探测）。
	// 故无 mock 时跳过控制面与会话面，写一条 SKIP 说明原因，其余骨架断言照跑。
	if !hasMock {
		rep.Add(check.Assertion{Level: check.LevelControl, ID: "control.skipped", Name: "控制面已跳过",
			Status: check.Skip,
			Detail: "未提供 conformance.json（无 mock 上游），控制面需要 mock 才能安全验证，已跳过（避免打到真实上游）"})
	} else {
		control(ctx, env, rep, d)
	}

	// ---- 会话面 + 多轮 + 用量 ----
	if !hasCap(rep.Conversation, "chat.generate") {
		rep.Add(check.Assertion{Level: check.LevelChat, ID: "chat.undeclared", Name: "声明 chat.generate",
			Status: check.Skip, Detail: "manifest 未声明 conversation=chat.generate（契约允许，渠道不可对话）"})
	} else if !hasMock {
		rep.Add(check.Assertion{Level: check.LevelChat, ID: "chat.skipped", Name: "会话面已跳过",
			Status: check.Skip,
			Detail: "未提供 conformance.json（无 mock 上游），对话需要 mock 上游才能安全验证，已跳过（避免打到真实上游）"})
	} else {
		model := d.Model
		if model == "" {
			model = firstModel(ctx, env)
		}
		if model == "" {
			rep.Add(check.Assertion{Level: check.LevelChat, ID: "chat.no-model", Name: "存在可用模型",
				Status: check.Fail, Detail: "models.list 未返回任何模型，无法测对话"})
		} else {
			chatBasics(ctx, env, rep, model, accountUID)
			if d.SingleTurn {
				singleTurnOnly(ctx, env, rep, model, accountUID)
			} else {
				multiTurn(ctx, env, rep, model, accountUID)
			}
			usageShape(ctx, env, rep, model, accountUID, d.NoTokenUsage)
		}
	}

	// ---- 拦截器（hook.stream 双向流）----
	//
	// 与渠道完全不同的形态：没有上游端点、没有模型、没有用量，
	// 所以 chat.* 断言对它无意义，走独立的 hook 层。
	if d.IsHook() {
		hookStream(ctx, env, rep, d.Hook)
	}

	// ---- 会话管理（web 逆向渠道）----
	if d.Session {
		session(ctx, env, rep)
	}

	// ---- 真实上游 ----
	if opts.Mode == check.ModeLive {
		live(ctx, env, rep, d, opts.LiveTurns)
	}

	rep.Ended = time.Now()
	return rep
}

func hasCap(caps []string, name string) bool {
	for _, c := range caps {
		if c == name {
			return true
		}
	}
	return false
}

func setupMock(env *harness.Env, d Descriptor) error {
	routes := d.Routes
	// 先起一个空 mock 拿到地址，再让 RoutesFor 生成路由。
	if d.RoutesFor != nil {
		s, err := mock.New(nil)
		if err != nil {
			return err
		}
		if err := s.Start(); err != nil {
			return err
		}
		env.Mock = s
		if env.Config == nil {
			env.Config = map[string]any{}
		}
		env.Config["base_url"] = s.Base
		if d.ExtraConfig != nil {
			for k, v := range d.ExtraConfig {
				env.Config[k] = v
			}
		}
		routes = d.RoutesFor(s.Base)
		env.BaseRoutes = routes
		return s.SetRoutes(routes)
	}
	s, err := mock.New(routes)
	if err != nil {
		return err
	}
	if err := s.Start(); err != nil {
		return err
	}
	env.Mock = s
	env.BaseRoutes = routes
	if env.Config == nil {
		env.Config = map[string]any{}
	}
	env.Config["base_url"] = s.Base
	if d.ExtraConfig != nil {
		for k, v := range d.ExtraConfig {
			env.Config[k] = v
		}
	}
	return nil
}

// firstModel 从 models.list 取第一个模型 ID。
func firstModel(ctx context.Context, env *harness.Env) string {
	res, err := env.CallWithConfig(ctx, "models.list", nil, harness.DefaultTimeout)
	if err != nil || res.Err != nil || !res.OK {
		return ""
	}
	ms, _ := res.Obj["models"].([]any)
	for _, it := range ms {
		if mm, ok := it.(map[string]any); ok {
			if id := str(mm, "id"); id != "" {
				return id
			}
		}
	}
	return ""
}

// ---- JSON 取值小工具 ----

// str 按路径取字符串（path 是逐层键名，不是「键 + 默认值」）。
func str(m map[string]any, path ...string) string {
	v := dig(m, path...)
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// strOr 按路径取字符串，缺失时返回 def。
func strOr(m map[string]any, def string, path ...string) string {
	if s := str(m, path...); s != "" {
		return s
	}
	return def
}

func strSlice(m map[string]any, path ...string) []string {
	v := dig(m, path...)
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, it := range arr {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func num(m map[string]any, path ...string) (float64, bool) {
	v := dig(m, path...)
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

// jsonBody 把 mock 收到的请求体解析成 map（解析失败返回 nil）。
func jsonBody(r *mock.Req) map[string]any {
	if r == nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Body), &m); err != nil {
		return nil
	}
	return m
}

// bodyLen 返回最后一次命中某路径的请求体长度。
func bodyLen(env *harness.Env, pathSub string) int {
	if env.Mock == nil {
		return 0
	}
	best := 0
	for _, r := range env.Mock.Requests() {
		if strings.Contains(r.Path, pathSub) && len(r.Body) > best {
			best = len(r.Body)
		}
	}
	return best
}

// findJSONRequest 找第一个路径匹配且 body 是 JSON 的请求。
func findJSONRequest(env *harness.Env, pathSub string) (*mock.Req, map[string]any) {
	if env.Mock == nil {
		return nil, nil
	}
	for _, r := range env.Mock.Requests() {
		if !strings.Contains(r.Path, pathSub) {
			continue
		}
		if m := jsonBody(&r); m != nil {
			rr := r
			return &rr, m
		}
	}
	return nil, nil
}

// evNames 返回事件名序列。
func evNames(res *wire.Result) []string {
	out := make([]string, 0, len(res.Events))
	for _, e := range res.Events {
		out = append(out, e.Name)
	}
	return out
}

// exists 判断路径是否存在（文件或目录）。
func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// absPath 把相对路径解析到 conformance 仓根。
func absPath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	// 从当前工作目录向上找 go.mod（module github.com/wild-work3/ww3-conformance）
	dir, _ := os.Getwd()
	for i := 0; i < 6 && dir != "" && dir != "/" && dir != `\`; i++ {
		if exists(filepath.Join(dir, "go.mod")) {
			return filepath.Join(dir, p)
		}
		dir = filepath.Dir(dir)
	}
	return p
}

var _ = fmt.Sprintf
