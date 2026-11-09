// descriptor.go 把「渠道事实」从代码里搬出去：工具核心零渠道知识。
//
// 三层可测深度（PLAN §2）：
//
//	A 层（零配置）     只给 --exe → Auto()：spawn `manifest` 取 id/kind/version，
//	                   无 Routes ⇒ suite.Run 自动跳过控制面与会话面（不打真上游）
//	B 层（自带描述文件）conformance.json → LoadFile()：作者声明 mock 路由/夹具模型/能力缺口
//	C 层（工作区预设）  presets/*.json → LoadFile()：同一个解析器，只是文件由本仓维护
//
// 关键纪律：本文件与 suite 包**不得出现任何渠道名**。渠道名只允许出现在
// presets/*.json 与作者自己的 conformance.json 里（数据，不是代码）。
package suite

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/wild-work3/ww3-conformance/internal/harness"
	"github.com/wild-work3/ww3-conformance/internal/mock"
)

// DescriptorSchema 是 conformance.json 的 schema 版本（前向兼容锚点）。
//
// 加字段一律 append-only；破坏性改动必须升到 2 并让旧值报错
// （与注册表 schema:2 同一条纪律，PLAN §6.1）。
const DescriptorSchema = 1

// descriptorFile 是 conformance.json 的磁盘形态。
type descriptorFile struct {
	Schema       int              `json:"schema"`
	ID           string           `json:"id"`
	Kind         string           `json:"kind"`
	Model        string           `json:"model"`
	LiveModel    string           `json:"live_model"`
	Auth         map[string]any   `json:"auth"`
	Account      map[string]any   `json:"account"`
	AuthName     string           `json:"auth_name"`
	AccountUID   string           `json:"account_uid"`
	ExtraConfig  map[string]any   `json:"extra_config"`
	Routes       []routeFile      `json:"routes"`
	SingleTurn   bool             `json:"single_turn"`
	NoTokenUsage bool             `json:"no_token_usage"`
	Session      bool             `json:"session"`
	SessionCID   string           `json:"session_cid"`
	SessionMID   string           `json:"session_mid"`
	AuthPrefixes []string         `json:"auth_prefixes"`
	LiveAnon     bool             `json:"live_anon"`
	Notes        []string         `json:"notes"`
	Hook         *hookConfig      `json:"hook"`
}

// routeFile 是 mock.Route 的 JSON 形态。
type routeFile struct {
	Method  string   `json:"method"`
	Path    string   `json:"path"`
	Kind    string   `json:"kind"`
	Status  int      `json:"status"`
	Body    string   `json:"body"`
	Text    string   `json:"text"`
	DelayMs int      `json:"delay_ms"`
	Chunks  int      `json:"chunks"`
	NoUsage bool     `json:"no_usage"`
	Usage   *usageJp `json:"usage"`
}

type usageJp struct {
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	Cached     int `json:"cached_tokens"`
	Write      int `json:"cache_write_tokens"`
}

// mockKinds 是 internal/mock 支持的响应形态（与 mock.Kind 常量一一对应）。
// 写成表而不是 switch 里现算，是为了在 LoadFile 时**提前**拒绝未知值，
// 而不是等到跑用例时才在 mock 层报一个难以定位的错。
var mockKinds = map[string]bool{
	string(mock.KindJSON):         true,
	string(mock.KindRaw):          true,
	string(mock.KindSSEOpenAI):    true,
	string(mock.KindSSEAnthropic): true,
	string(mock.KindSSENested):    true,
	string(mock.KindSSEText):      true,
	string(mock.KindSSERaw):       true,
	string(mock.KindStatus):       true,
}

// LoadFile 读取一份 conformance.json（作者自带 或 presets/ 下的预设）。
//
// exe 与 args 由调用方给（描述文件不描述「怎么启动我」——那是调用现场的事实）。
func LoadFile(path, exe string, args []string) (Descriptor, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Descriptor{}, fmt.Errorf("读描述文件失败: %w", err)
	}
	var f descriptorFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return Descriptor{}, fmt.Errorf("描述文件不是合法 JSON: %w", err)
	}
	if f.Schema != DescriptorSchema {
		return Descriptor{}, fmt.Errorf("描述文件 schema=%d 不受支持（本工具只认 %d）", f.Schema, DescriptorSchema)
	}
	d := Descriptor{
		ID: f.ID, Kind: f.Kind, Exe: exe, Args: args,
		Model: f.Model, LiveModel: f.LiveModel,
		Auth: f.Auth, Account: f.Account,
		AuthName: f.AuthName, AccountUID: f.AccountUID,
		ExtraConfig:  f.ExtraConfig,
		SingleTurn:   f.SingleTurn,
		NoTokenUsage: f.NoTokenUsage,
		Session:      f.Session,
		SessionCID:   f.SessionCID,
		SessionMID:   f.SessionMID,
		AuthPrefixes: f.AuthPrefixes,
		LiveAnon:     f.LiveAnon,
		Notes:        f.Notes,
		Hook:         f.Hook,
	}
	for i, r := range f.Routes {
		if !mockKinds[r.Kind] {
			return Descriptor{}, fmt.Errorf("routes[%d].kind=%q 不是支持的形态（可选：json/raw/sse_openai/sse_anthropic/sse_nested/sse_text/sse_raw/status）", i, r.Kind)
		}
		if _, err := regexp.Compile(r.Path); err != nil {
			return Descriptor{}, fmt.Errorf("routes[%d].path 不是合法正则: %v", i, err)
		}
		mr := mock.Route{
			Method: r.Method, Path: r.Path, Kind: mock.Kind(r.Kind), Status: r.Status,
			Body: r.Body, Text: r.Text, Chunks: r.Chunks, NoUsage: r.NoUsage,
		}
		if r.DelayMs > 0 {
			mr.Delay = time.Duration(r.DelayMs) * time.Millisecond
		}
		if r.Usage != nil {
			mr.Usage = &mock.Usage{Prompt: r.Usage.Prompt, Completion: r.Usage.Completion,
				Cached: r.Usage.Cached, Write: r.Usage.Write}
		}
		d.Routes = append(d.Routes, mr)
	}
	if err := d.Validate(); err != nil {
		return Descriptor{}, err
	}
	return d, nil
}

// Auto 构造 A 层描述符：只 spawn `manifest` 拿身份，**不带 Routes**。
//
// 没有 mock ⇒ suite.Run 会跳过控制面与会话面（A 层硬边界：绝不静默打真上游）。
// 这是「任何插件仓 CI 都能加的一步」：零配置、零网络。
func Auto(ctx context.Context, exe string, args []string) (Descriptor, error) {
	if exe == "" {
		return Descriptor{}, fmt.Errorf("--exe 必填")
	}
	if st, err := os.Stat(exe); err != nil {
		return Descriptor{}, fmt.Errorf("--exe 指向的文件不存在: %s", exe)
	} else if st.IsDir() {
		return Descriptor{}, fmt.Errorf("--exe 指向的是目录而不是可执行文件: %s", exe)
	}
	// 用临时夹具跑 manifest（插件会建 <data>/{auths,logs,stat}，不能落在当前目录）
	root, err := os.MkdirTemp("", "ww3-conf-auto-")
	if err != nil {
		return Descriptor{}, err
	}
	defer os.RemoveAll(root)

	d := Descriptor{Exe: exe, Args: args}
	m, err := manifestOf(ctx, root, exe, args)
	if err != nil {
		return Descriptor{}, fmt.Errorf("取 manifest 失败（--exe 是否真的是 ww3 插件？）: %w", err)
	}
	d.ID = strOr(m, "", "id")
	d.Kind = strOr(m, "channel", "kind")
	if d.ID == "" {
		return Descriptor{}, fmt.Errorf("manifest 缺少 id 字段，无法确定插件身份")
	}
	d.Notes = []string{
		fmt.Sprintf("A 层（零配置）：只验契约骨架；控制面/会话面已跳过（未提供 conformance.json，避免打到真实上游）"),
		fmt.Sprintf("manifest: name=%s version=%s kind=%s", str(m, "name"), str(m, "version"), d.Kind),
	}
	return d, nil
}

// manifestOf 在一个临时夹具里跑 `manifest` 并解析。
func manifestOf(ctx context.Context, root, exe string, args []string) (map[string]any, error) {
	env, err := harness.NewEnv(root, "auto", exe, args)
	if err != nil {
		return nil, err
	}
	defer env.Close()
	return env.Manifest(ctx)
}

// Validate 校验描述符自洽性（在 LoadFile / Auto 之后调用）。
//
// 只做**结构性**校验：渠道事实对不对（路由写没写对、模型名真不真）工具无法判断，
// 那是作者的责任——描述文件里的 notes/single_turn 等必须诚实填写（PLAN §3.2）。
func (d *Descriptor) Validate() error {
	switch d.Kind {
	case "", "channel", "hook":
	default:
		return fmt.Errorf("kind=%q 非法（可选：channel | hook）", d.Kind)
	}
	if d.Kind == "hook" && len(d.Routes) > 0 {
		return fmt.Errorf("hook 插件不接受 routes（拦截器没有上游端点，不该有 mock 路由表）")
	}
	// ID 允许留空：suite.Run 会从 manifest.id 回填（作者不必在描述文件里重复一遍身份）。
	return nil
}

// IsHook 报告是否是拦截器插件。
func (d Descriptor) IsHook() bool { return d.Kind == "hook" }

// presetDir 是内置预设目录（C 层）。
//
// 注意：presets/ 是**数据目录**，不是代码——它们是改造前硬编码渠道事实的一次性导出物。
// 工具本身不 import 任何渠道名，删掉整个 presets/ 也照样能跑 --exe。
func presetDir() string { return absPath("presets") }

// LoadPreset 载入内置预设（C 层，等价于原来的 --only <渠道>）。
func LoadPreset(name, exe string, args []string) (Descriptor, error) {
	p := filepath.Join(presetDir(), name+".json")
	if !exists(p) {
		return Descriptor{}, fmt.Errorf("没有内置预设 %q（预设目录: %s）", name, presetDir())
	}
	return LoadFile(p, exe, args)
}

// ListPresets 返回内置预设名（按文件名排序）。
func ListPresets() []string {
	ents, err := os.ReadDir(presetDir())
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if n := e.Name(); filepath.Ext(n) == ".json" {
			out = append(out, n[:len(n)-5])
		}
	}
	return out
}
