// ui.go 是 Web 界面层（W3-61：ui slot + serve 命令）的通用断言（阶段 5）。
//
// 为什么单独一层：serve 不是 JSONL-RPC 会话而是「就绪帧 + 长驻回环 HTTP」，
// 与 call/stdio/hook.stream 都不同形态；且**零上游网络**（只打 127.0.0.1），
// 所以它是唯一能在 A 层（零配置）安全跑的运行时断言——控制面/会话面因可能
// 打到真实上游而必须跳过，UI 层不用。
//
// 契约权威：protocol/docs/PLUGIN_PROTOCOL.md §3.1.1（ui slot / entry / 资源自包含）、
// protocol/docs/PLUGIN_CLI.md §2（serve 命令 / serve-ready 帧 / healthz）。
//
// 判定纪律与 hook 层一致：未声明 ui → SKIP（契约允许，0~N 个 slot）；
// 声明了 ui 就必须把 serve 契约全跑通（缺项 FAIL）。
package suite

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/wild-work3/ww3-conformance/internal/check"
	"github.com/wild-work3/ww3-conformance/internal/harness"
	"github.com/wild-work3/ww3-conformance/internal/wire"
)

const uiContract = "PLUGIN_PROTOCOL §3.1.1 / PLUGIN_CLI §2"

// uiSlot 是 manifest.ui 里声明的一个 slot。
type uiSlot struct {
	Pos   string // main | settings
	ID    string
	Title string
	Entry string
}

// uiServe 跑 UI 层全部断言。m 是已取得的 manifest（suite.Run 传入）。
func uiServe(ctx context.Context, env *harness.Env, rep *check.PluginReport, m map[string]any, cfg *uiConfig) {
	slots, uiErr := parseUI(m)
	if uiErr != "" || len(slots) == 0 {
		// 未声明 ui：契约允许。但「实现了 serve 却忘了声明」值得一条 WARN——
		// 那种插件在 gateway 下永远不会被拉起，作者多半以为功能已生效。
		if warnServeWithoutUI(ctx, env) {
			rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.declared", Name: "声明 ui slot",
				Status:   check.Warn,
				Detail:   "manifest 未声明 ui，但 serve 命令可启动（gateway 不会拉起没有 slot 的插件，声明会被浪费）",
				Contract: uiContract})
		} else {
			rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.declared", Name: "声明 ui slot",
				Status: check.Skip, Detail: "manifest 未声明 ui（契约允许：无 Web 界面）", Contract: uiContract})
		}
		return
	}
	rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.declared", Name: "声明 ui slot",
		Status: check.Pass, Detail: fmt.Sprintf("%d 个 slot（main=%d settings=%d）",
			len(slots), countPos(slots, "main"), countPos(slots, "settings")), Contract: uiContract})

	// ---- ui.shape：slot 字段合法性（纯静态，无进程）----
	uiShape(rep, slots)

	// ---- serve 契约（全部跑在同一份就绪进程上）----
	proc, err := spawnServeFor(ctx, env, "127.0.0.1:0")
	if err != nil {
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.ready", Name: "serve 就绪帧",
			Status: check.Fail, Detail: err.Error(), Contract: uiContract})
		return
	}
	defer func() { proc.Kill(); proc.Wait(5 * time.Second) }() // 防孤儿（W3-18）

	host, _, perr := splitListen(proc.Ready.Listen)
	if perr != nil {
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.ready", Name: "serve 就绪帧",
			Status: check.Fail, Detail: perr.Error(), Contract: uiContract})
		return
	}
	rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.ready", Name: "serve 就绪帧",
		Status: check.Pass, Detail: proc.ReadyLine, Contract: uiContract})

	// ---- ui.serve.loopback：就绪帧必须是回环地址；0.0.0.0 必须被拒绝 ----
	if host == "127.0.0.1" || host == "localhost" {
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.loopback", Name: "只监听回环地址",
			Status: check.Pass, Detail: "listen=" + proc.Ready.Listen, Contract: uiContract})
	} else {
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.loopback", Name: "只监听回环地址",
			Status: check.Fail, Detail: "listen host=" + host + "（契约：仅 127.0.0.1，插件 Web 只经 gateway 反代）", Contract: uiContract})
	}
	// 负例：--listen 0.0.0.0:PORT 必须非 0 退出（拒绝绑定非回环是安全硬约束）
	if _, _, code, _ := env.RunCmd(ctx, []string{"serve", "--data", env.DataDir, "--listen", "0.0.0.0:0"}, nil, 10*time.Second); code == 0 {
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.loopback.reject", Name: "拒绝非回环监听",
			Status: check.Fail, Detail: "--listen 0.0.0.0:0 竟然成功（rc=0）", Contract: uiContract})
	} else {
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.loopback.reject", Name: "拒绝非回环监听",
			Status: check.Pass, Detail: "--listen 0.0.0.0:0 被拒绝（非 0 退出）", Contract: uiContract})
	}

	// ---- ui.serve.quiet：就绪帧之后 stdout 必须静默 ----
	time.Sleep(1200 * time.Millisecond)
	if extra := proc.ExtraLines(); len(extra) > 0 {
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.quiet", Name: "就绪帧后 stdout 静默",
			Status: check.Fail, Detail: fmt.Sprintf("就绪帧后多了 %d 行输出（gateway 靠 stdout 首帧定位端口，日志必须走 stderr/文件）: %s",
				len(extra), truncStr(strings.Join(extra, " | "), 200)), Contract: uiContract})
	} else if !proc.StdoutEOF() && proc.Alive() {
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.quiet", Name: "就绪帧后 stdout 静默",
			Status: check.Pass, Detail: "1.2s 无新增输出且进程存活", Contract: uiContract})
	} else {
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.quiet", Name: "就绪帧后 stdout 静默",
			Status: check.Fail, Detail: "serve 进程在就绪后退出/关闭了 stdout（stderr: " + truncStr(proc.Stderr(), 300) + "）", Contract: uiContract})
	}

	base := "http://" + proc.Ready.Listen
	client := &http.Client{Timeout: 5 * time.Second}

	// ---- ui.serve.healthz：200 且两次一致（纯本地无副作用探针）----
	b1, st1, err1 := httpGet(client, base+"/healthz")
	b2, st2, err2 := httpGet(client, base+"/healthz")
	switch {
	case err1 != nil:
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.healthz", Name: "/healthz 探针",
			Status: check.Fail, Detail: "GET /healthz 失败: " + err1.Error(), Contract: uiContract})
	case err2 != nil:
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.healthz", Name: "/healthz 探针",
			Status: check.Fail, Detail: "第二次 GET /healthz 失败: " + err2.Error(), Contract: uiContract})
	case st1 != 200:
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.healthz", Name: "/healthz 探针",
			Status: check.Fail, Detail: fmt.Sprintf("GET /healthz 状态码 %d（gateway 探活要求 200）", st1), Contract: uiContract})
	case b1 != b2 || st1 != st2:
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.healthz", Name: "/healthz 探针",
			Status: check.Fail, Detail: "两次调用结果不一致（探针必须无副作用）", Contract: uiContract})
	default:
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.healthz", Name: "/healthz 探针",
			Status: check.Pass, Detail: "200，两次一致", Contract: uiContract})
	}

	// ---- ui.serve.entry：每个 slot 的 entry 必须可取（声明了就得给得出页面）----
	for _, s := range slots {
		body, ct, status, err := httpGetType(client, base+"/"+s.Entry)
		id := "ui.serve.entry." + s.Pos + "." + s.ID
		switch {
		case err != nil:
			rep.Add(check.Assertion{Level: check.LevelUI, ID: id, Name: "slot 页面可访问",
				Status: check.Fail, Detail: fmt.Sprintf("GET /%s 失败: %v（声明了 slot 但页面不存在）", s.Entry, err), Contract: uiContract})
		case status != 200:
			rep.Add(check.Assertion{Level: check.LevelUI, ID: id, Name: "slot 页面可访问",
				Status: check.Fail, Detail: fmt.Sprintf("GET /%s 状态码 %d（声明了 slot 但页面不存在）", s.Entry, status), Contract: uiContract})
		case !strings.Contains(ct, "text/html"):
			rep.Add(check.Assertion{Level: check.LevelUI, ID: id, Name: "slot 页面可访问",
				Status: check.Fail, Detail: fmt.Sprintf("GET /%s Content-Type=%q（应为 text/html）", s.Entry, ct), Contract: uiContract})
		default:
			rep.Add(check.Assertion{Level: check.LevelUI, ID: id, Name: "slot 页面可访问",
				Status: check.Pass, Detail: fmt.Sprintf("/%s 200 text/html（%d 字节）", s.Entry, len(body)), Contract: uiContract})
		}
	}

	// ---- ui.serve.selfcontained：entry HTML 不得引用根绝对路径资源 ----
	// 契约明说这是插件作者的责任、core 无法从反代侧侦测修复——所以只能在插件侧测。
	// 根绝对路径在直连调试时能显示，集成后必 404，是最典型的「集成才炸」缺陷。
	for _, s := range slots {
		body, _, status, err := httpGetType(client, base+"/"+s.Entry)
		id := "ui.serve.selfcontained." + s.Pos + "." + s.ID
		if err != nil || status != 200 {
			continue // entry 断言已报 FAIL，这里不重复
		}
		if hits := scanRootAbsolute(string(body)); len(hits) > 0 {
			rep.Add(check.Assertion{Level: check.LevelUI, ID: id, Name: "资源自包含（相对路径）",
				Status: check.Fail, Detail: fmt.Sprintf("/%s 引用了根绝对路径资源 %v（gateway 反代剥前缀后这些路径会落到宿主，集成必 404）",
					s.Entry, hits), Contract: uiContract})
		} else {
			rep.Add(check.Assertion{Level: check.LevelUI, ID: id, Name: "资源自包含（相对路径）",
				Status: check.Pass, Detail: "entry HTML 无 src/href/url() 根绝对路径引用", Contract: uiContract})
		}
	}

	// ---- ui.serve.probes：插件专属 HTTP API 自证（描述文件声明，可缺省）----
	var probes []uiProbe
	if cfg != nil {
		probes = cfg.Probes
	}
	if len(probes) > 0 {
		for i, p := range probes {
			path := "/" + strings.TrimPrefix(p.Path, "/")
			want := p.WantStatus
			if want == 0 {
				want = 200
			}
			body, status, err := httpGet(client, base+path)
			id := fmt.Sprintf("ui.serve.probe.%d", i)
			switch {
			case err != nil:
				rep.Add(check.Assertion{Level: check.LevelUI, ID: id, Name: "插件 API " + path,
					Status: check.Fail, Detail: "请求失败: " + err.Error(), Contract: "conformance.json ui.probes"})
			case status != want:
				rep.Add(check.Assertion{Level: check.LevelUI, ID: id, Name: "插件 API " + path,
					Status: check.Fail, Detail: fmt.Sprintf("状态码 %d（作者声明期望 %d）", status, want), Contract: "conformance.json ui.probes"})
			case p.BodyContains != "" && !strings.Contains(body, p.BodyContains):
				rep.Add(check.Assertion{Level: check.LevelUI, ID: id, Name: "插件 API " + path,
					Status: check.Fail, Detail: fmt.Sprintf("响应体不含 %q（前 200 字节: %s）", p.BodyContains, truncStr(body, 200)), Contract: "conformance.json ui.probes"})
			default:
				rep.Add(check.Assertion{Level: check.LevelUI, ID: id, Name: "插件 API " + path,
					Status: check.Pass, Detail: fmt.Sprintf("%d，%d 字节", status, len(body)), Contract: "conformance.json ui.probes"})
			}
		}
	} else {
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.probes", Name: "插件专属 API 自证",
			Status: check.Skip, Detail: "描述文件未声明 ui.probes（可选）", Contract: "conformance.json ui.probes"})
	}

	// ---- ui.serve.exit：终止后退出、无残留 ----
	proc.Kill()
	code, werr := proc.Wait(5 * time.Second)
	if werr != nil && !proc.Alive() {
		// Windows 上强杀后 ProcessState 可能为 nil，只要确认死了就算过
		code = 0
		werr = nil
	}
	if werr != nil {
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.exit", Name: "终止后退出无残留",
			Status: check.Fail, Detail: werr.Error(), Contract: uiContract})
	} else {
		note := ""
		if !isUnixLike() {
			// Windows 无法向子进程发 SIGTERM（Go 的 Process.Kill 是强杀），
			// 「优雅退出」语义在这里只能降级为「能终止、无残留」，诚实写明。
			note = "（Windows 无法发 SIGTERM，优雅退出未验证；POSIX 下应响应 SIGTERM 退出码 0）"
		}
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.serve.exit", Name: "终止后退出无残留",
			Status: check.Pass, Detail: fmt.Sprintf("已终止（exit=%d），无残留进程%s", code, note), Contract: uiContract})
	}
}

// warnServeWithoutUI 探测「没声明 ui 却实现了 serve」。启动成功 → true（值得 WARN）。
// 未实现 serve（rc≠2 之外的快速失败）或超时 → false。探测进程用完即杀。
func warnServeWithoutUI(ctx context.Context, env *harness.Env) bool {
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	proc, err := wire.SpawnServe(cctx, env.ID, env.Exe,
		append(append([]string{}, env.Args...), "serve", "--data", env.DataDir, "--listen", "127.0.0.1:0"), 3*time.Second)
	if err != nil {
		return false
	}
	proc.Kill()
	proc.Wait(3 * time.Second)
	return true
}

// spawnServeFor 用 harness 的 argv 口径拉起 serve。
func spawnServeFor(ctx context.Context, env *harness.Env, listen string) (*wire.ServeProc, error) {
	argv := append(append([]string{}, env.Args...), "serve", "--data", env.DataDir, "--listen", listen)
	return wire.SpawnServe(ctx, env.ID, env.Exe, argv, 10*time.Second)
}

// parseUI 从 manifest 解析 ui slot。返回 slots 与错误描述（空串 = 无 ui 或合法）。
func parseUI(m map[string]any) ([]uiSlot, string) {
	raw, ok := m["ui"].(map[string]any)
	if !ok {
		return nil, ""
	}
	var out []uiSlot
	for _, pos := range []string{"main", "settings"} {
		arr, _ := raw[pos].([]any)
		for _, it := range arr {
			o, ok := it.(map[string]any)
			if !ok {
				return nil, fmt.Sprintf("ui.%s 含非对象元素", pos)
			}
			out = append(out, uiSlot{
				Pos: pos, ID: str(o, "id"), Title: str(o, "title"), Entry: str(o, "entry"),
			})
		}
	}
	return out, ""
}

// uiShape 校验 slot 字段：entry 合法相对路径、同位置 id 唯一、title 纯文本非空。
func uiShape(rep *check.PluginReport, slots []uiSlot) {
	var bad []string
	seen := map[string]bool{}
	for _, s := range slots {
		key := s.Pos + "/" + s.ID
		if s.ID == "" {
			bad = append(bad, s.Pos+" 段有 slot 缺 id")
		} else if seen[key] {
			bad = append(bad, "同位置重复 slot id: "+key)
		}
		seen[key] = true
		if s.Title == "" {
			bad = append(bad, s.ID+" 缺 title")
		} else if strings.ContainsAny(s.Title, "<>&") {
			bad = append(bad, s.ID+" 的 title 含 HTML 字符（应为纯文本）")
		}
		if !validEntry(s.Entry) {
			bad = append(bad, fmt.Sprintf("%s 的 entry=%q 非法（须为插件 HTTP 根下的纯相对路径，无 / 开头、无 .. 与 ?#）", s.ID, s.Entry))
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.shape", Name: "slot 字段合法性",
			Status: check.Fail, Detail: strings.Join(bad, "；"), Contract: uiContract})
	} else {
		rep.Add(check.Assertion{Level: check.LevelUI, ID: "ui.shape", Name: "slot 字段合法性",
			Status: check.Pass, Detail: "id 唯一、title 纯文本、entry 均为合法相对路径", Contract: uiContract})
	}
}

// validEntry 与主仓 meta.ValidEntry 同口径（独立实现校验契约，理由同 wire）：
// 非空、无前导 /、无 \\、无 ? #、无协议、无 .. / . / 空段。
func validEntry(e string) bool {
	if e == "" || strings.HasPrefix(e, "/") {
		return false
	}
	if strings.ContainsAny(e, "\\?#") || strings.Contains(e, "://") {
		return false
	}
	for _, seg := range strings.Split(e, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// 根绝对路径资源引用：src="/x"、href="/x"、url(/x)、url('/x')。
// 首字符排除 / 与引号类，避免把协议相对（//cdn）和自引用（href="/"）误报。
var rootAbsRe = regexp.MustCompile(`(?i)(?:\bsrc|\bhref)\s*=\s*["'](/[^/"'][^"']*)["']`)
var rootAbsURLRe = regexp.MustCompile(`(?i)url\(\s*["']?(/[^/"')][^"')]*)["']?\s*\)`)

// scanRootAbsolute 扫出 entry HTML 里的根绝对路径资源引用。
// 已知局限（写进报告即可）：JS 里动态拼接的 fetch('/api/...') 无法静态发现，
// 集成前请用 `--ui-proxy` 手动联调模式实测。
func scanRootAbsolute(html string) []string {
	seen := map[string]bool{}
	var out []string
	for _, re := range []*regexp.Regexp{rootAbsRe, rootAbsURLRe} {
		for _, m := range re.FindAllStringSubmatch(html, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	return out
}

// splitListen 拆 "127.0.0.1:57530"。
func splitListen(s string) (host, port string, err error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", "", fmt.Errorf("listen 缺端口: %q", s)
	}
	return s[:i], s[i+1:], nil
}

// httpGet 简单 GET，返回 body 与状态码。
func httpGet(c *http.Client, url string) (string, int, error) {
	resp, err := c.Get(url)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", resp.StatusCode, err
	}
	return string(b), resp.StatusCode, nil
}

// httpGetType 同 httpGet 但连 Content-Type 一起返回。
func httpGetType(c *http.Client, url string) (body, ctype string, status int, err error) {
	resp, err := c.Get(url)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", resp.Header.Get("Content-Type"), resp.StatusCode, err
	}
	return string(b), resp.Header.Get("Content-Type"), resp.StatusCode, nil
}

func countPos(slots []uiSlot, pos string) int {
	n := 0
	for _, s := range slots {
		if s.Pos == pos {
			n++
		}
	}
	return n
}

// isUnixLike 报告当前平台能否发 SIGTERM（Windows 不能）。
func isUnixLike() bool { return runtime.GOOS != "windows" }
