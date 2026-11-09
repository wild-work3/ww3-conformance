// Package harness 提供准入测试的插件夹具：临时数据目录、凭据、mock 上游、
// 以及「一次性 call 进程」与「常连 stdio 进程」两种调用形态。
//
// 设计约束（W3-20 / W3-21）：
//   - 渠道插件无常驻形态，core 一律用 `call` 一请求一进程；准入测试必须走同一条路，
//     不能为了测试方便就绕过 `call` 直接调内部函数（否则测出来的不是契约）。
//   - 插件数据根是插件安装根 `<plugins>/<id>`，凭据在 `<data>/auths/`、
//     日志在 `<data>/logs/`、统计在 `<data>/stat/`。
//   - 渠道配置在 `chat.generate` 的 `channel_config` 字段里下发（陷阱 20），
//     控制面调用则放在 params 的 `config` 字段。
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wild-work3/ww3-conformance/internal/mock"
	"github.com/wild-work3/ww3-conformance/internal/wire"
)

// DefaultTimeout 是一次性调用的缺省超时。
// 控制面 30s 足够；对话类由调用方显式给更长的值（ChatTimeout 量级）。
const DefaultTimeout = 30 * time.Second

// ChatTimeout 是对话类调用的超时（与 core 的 ChatTimeout=300s 对齐）。
const ChatTimeout = 300 * time.Second

// Env 是一个插件的测试夹具。
type Env struct {
	Root string // 临时根目录
	// DataDir 是插件数据目录（相当于 <plugins>/<id>）。
	DataDir string
	// Exe 是插件可执行文件绝对路径（可以是解释器，如 python.exe）。
	Exe string
	// Args 是透传给被测进程的前缀参数（W3-51 `--` 之后的参数）。
	// 单 exe 插件为空；Python/Node 插件形如 ["main.py"]。
	// 契约子命令与 `--data` 追加在其后：argv = Exe + Args + [子命令, "--data", DataDir]。
	Args []string
	// ID 是插件 ID。
	ID string
	// Mock 是本地 mock 上游（可为 nil）。
	Mock *mock.Server
	// Config 是控制面/会话面都要带上的渠道配置（含 base_url）。
	Config map[string]any
	// BaseRoutes 是 mock 初始路由（用于在用例间切换路由后恢复）。
	BaseRoutes []mock.Route
	// AuthName 是默认凭据名（便利命令 --auth 用）。
	AuthName string
	// CID 是会话管理用例要用的会话 ID（由描述符提供）。
	CID string
	// MID 是按消息操作的会话命令（conv-read）要用的消息 ID（由描述符提供）。
	MID string
}

// UseUsage 把 mock 的 SSE 路由改成「报 usage」形态。
func (e *Env) UseUsage(u *mock.Usage) error {
	if e.Mock == nil || e.BaseRoutes == nil {
		return nil
	}
	rs := make([]mock.Route, len(e.BaseRoutes))
	copy(rs, e.BaseRoutes)
	for i := range rs {
		rs[i].Usage = u
		rs[i].NoUsage = false
	}
	return e.Mock.SetRoutes(rs)
}

// UseNoUsage 把 mock 的 SSE 路由改成「上游不报 usage」形态。
func (e *Env) UseNoUsage() error {
	if e.Mock == nil || e.BaseRoutes == nil {
		return nil
	}
	rs := make([]mock.Route, len(e.BaseRoutes))
	copy(rs, e.BaseRoutes)
	for i := range rs {
		rs[i].NoUsage = true
		rs[i].Usage = nil
	}
	return e.Mock.SetRoutes(rs)
}

// NewEnv 创建夹具并建立数据目录骨架。
// args 是透传给被测进程的前缀参数（单 exe 插件传 nil）。
func NewEnv(root, id, exe string, args []string) (*Env, error) {
	e := &Env{Root: root, ID: id, Exe: exe, Args: args, DataDir: filepath.Join(root, "data")}
	for _, sub := range []string{"auths", "logs", "stat", "auths/flows"} {
		if err := os.MkdirAll(filepath.Join(e.DataDir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("建数据目录 %s 失败: %w", sub, err)
		}
	}
	return e, nil
}

// StartMock 启动 mock 上游并把 base_url 写进 Config。
func (e *Env) StartMock(routes []mock.Route) error {
	s, err := mock.New(routes)
	if err != nil {
		return err
	}
	if err := s.Start(); err != nil {
		return err
	}
	e.Mock = s
	if e.Config == nil {
		e.Config = map[string]any{}
	}
	e.Config["base_url"] = s.Base
	return nil
}

// Close 释放 mock。
func (e *Env) Close() {
	if e.Mock != nil {
		e.Mock.Close()
	}
}

// WriteAuth 写入一份凭据（嵌套形 {auth,account}，W3-21 §5.1）。
// name 是文件名（不含 .json），也是 core 侧 account_uid。
func (e *Env) WriteAuth(name string, auth, account map[string]any) error {
	doc := map[string]any{}
	if auth != nil {
		doc["auth"] = auth
	}
	if account != nil {
		doc["account"] = account
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(e.DataDir, "auths", name+".json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		return fmt.Errorf("写凭据 %s 失败: %w", name, err)
	}
	return nil
}

// WriteAuthRaw 写入原始凭据字节（用于扁平形等特殊形状）。
func (e *Env) WriteAuthRaw(name string, raw []byte) error {
	p := filepath.Join(e.DataDir, "auths", name+".json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		return fmt.Errorf("写凭据 %s 失败: %w", name, err)
	}
	return nil
}

// WriteConfig 写插件的本地 config.json（部分插件用 <data>/config.json 存配置）。
func (e *Env) WriteConfig(obj any) error {
	raw, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(e.DataDir, "config.json"), raw, 0o600)
}

// argv 拼出「被测进程的完整命令行」：Exe + Args + sub + ("--data" DataDir)。
//
// 三处调用（manifest / call / stdio）必须走同一份拼装，否则 Python/Node 插件
// 会出现「某个子命令能用、另一个不能用」这类最难查的形态差异（W3-51 §3.1.1）。
func (e *Env) argv(sub string, withData bool) []string {
	out := make([]string, 0, len(e.Args)+4)
	out = append(out, e.Args...)
	out = append(out, sub)
	if withData {
		out = append(out, "--data", e.DataDir)
	}
	return out
}

// RunCmd 以任意子命令跑一次插件进程，返回 stdout/stderr/退出码。
// stdin 为 nil 时不写输入。args 是**子命令之后**的参数（不含 Exe/Args）。
func (e *Env) RunCmd(ctx context.Context, args []string, stdin []byte, timeout time.Duration) (stdout, stderr []byte, code int, err error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := append(append([]string{}, e.Args...), args...)
	return wire.RunOnce(cctx, e.Exe, argv, stdin, nil, timeout)
}

// Manifest 跑 `manifest` 子命令并解析 JSON。
func (e *Env) Manifest(ctx context.Context) (map[string]any, error) {
	out, errb, code, err := e.RunCmd(ctx, []string{"manifest"}, nil, DefaultTimeout)
	if err != nil {
		return nil, fmt.Errorf("manifest 执行失败: %w（stderr: %s）", err, truncate(string(errb), 400))
	}
	if code != 0 {
		return nil, fmt.Errorf("manifest 退出码 %d（stderr: %s）", code, truncate(string(errb), 400))
	}
	// manifest 可能把 JSON 打在 stdout；若插件把日志也写 stdout 则取最后一个 JSON 对象。
	body := extractJSON(string(out))
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return nil, fmt.Errorf("manifest 输出不是 JSON: %v（前 300 字节: %s）", err, truncate(string(out), 300))
	}
	return m, nil
}

// Call 以 `call` 形态跑一次方法调用（core 的真实调用方式）。
func (e *Env) Call(ctx context.Context, method string, params any, timeout time.Duration) (*wire.Result, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	frame := wire.InvokeFrames(wire.Call{Method: method, Params: params})
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, stderr, code, err := wire.RunOnce(cctx, e.Exe, e.argv("call", true), frame, nil, timeout)
	res := &wire.Result{Method: method, Stderr: string(stderr)}
	if err != nil && code == 0 {
		// 超时/启动失败
		res.Err = &wire.ErrObj{Kind: "harness", Scope: "request", Message: err.Error()}
		return res, err
	}
	frames, perr := wire.ParseFrames(stdout)
	if perr != nil {
		res.Err = &wire.ErrObj{Kind: "harness", Scope: "request",
			Message: fmt.Sprintf("stdout 不是纯 JSONL: %v", perr)}
		return res, nil
	}
	fill(res, frames)
	if code != 0 && res.Err == nil {
		res.Err = &wire.ErrObj{Kind: "harness", Scope: "request",
			Message: fmt.Sprintf("进程退出码 %d（stderr: %s）", code, truncate(string(stderr), 300))}
	}
	return res, nil
}

// CallWithConfig 是 Call 的便捷包装：把 e.Config 放进 params.config。
func (e *Env) CallWithConfig(ctx context.Context, method string, extra map[string]any, timeout time.Duration) (*wire.Result, error) {
	p := map[string]any{}
	for k, v := range extra {
		p[k] = v
	}
	if len(e.Config) > 0 {
		p["config"] = e.Config
	}
	return e.Call(ctx, method, p, timeout)
}

// Chat 发起一次 chat.generate（params 里自动带 channel_config）。
func (e *Env) Chat(ctx context.Context, req map[string]any, timeout time.Duration) (*wire.Result, error) {
	p := map[string]any{}
	for k, v := range req {
		p[k] = v
	}
	if _, ok := p["channel_config"]; !ok && len(e.Config) > 0 {
		p["channel_config"] = e.Config
	}
	return e.Call(ctx, "chat.generate", p, timeout)
}

// Stdio 拉起常连 stdio 进程（用于契约骨架检查与 hook 双向流）。
func (e *Env) Stdio(ctx context.Context) (*wire.Client, error) {
	return wire.Spawn(ctx, e.ID, e.Exe, e.argv("stdio", true), nil)
}

// fill 把帧序列填进 Result。
func fill(res *wire.Result, frames []wire.Frame) {
	for _, f := range frames {
		switch f.Op {
		case wire.OpEvent:
			d := map[string]any{}
			_ = json.Unmarshal(f.Data, &d)
			res.Events = append(res.Events, wire.Event{Name: f.Event, Data: d, Raw: f.Data})
		case wire.OpResult:
			res.Raw = f.Result
			res.OK = f.OK != nil && *f.OK
			res.Err = f.Error
			if len(f.Result) > 0 {
				_ = json.Unmarshal(f.Result, &res.Obj)
			}
		}
	}
}

// extractJSON 从可能夹带日志的输出里取出 JSON 主体。
func extractJSON(s string) string {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "{") {
		// 取第一个 '{' 到最后一个 '}'
		if i := strings.LastIndex(t, "}"); i > 0 {
			return t[:i+1]
		}
	}
	// 逐行找以 { 开头的行起点
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "{") {
			joined := strings.Join(lines[i:], "\n")
			if j := strings.LastIndex(joined, "}"); j > 0 {
				return joined[:j+1]
			}
		}
	}
	return s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
