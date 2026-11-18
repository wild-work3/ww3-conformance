// Package uiproxy 是手动联调模式（PLAN 阶段 5）：把 gateway 的 /ext/<id>/* 反代机制
// 复刻给单个插件，让插件作者**不依赖 gateway + core** 就能调试自己的前端 UI。
//
// 定位（用户定案）：
//   - 这是**手动联调模式**，长驻运行、Ctrl+C 退出，**不产报告、不写摘要、退出码与准入判定解耦**；
//   - 不集成在自动化构建里（不进 build.sh / CI / publish.yml）；
//   - 其余 serve 模式下的 HTTP API 契约仍由 suite 的 UI 层自动验证（internal/suite/ui.go）。
//
// 与 gateway 行为的对齐（main/gateway/internal/proxy/proxy.go 的 /ext 分支）：
//   - /ext/<id>（无尾斜杠）→ 308 到尾斜杠（页面内相对路径按目录语义解析）；
//   - /ext/<id>/<rest> → 剥前缀转发 + 注入 X-WW3-Upstream: ext:<id> + FlushInterval:-1；
//   - 非法 id → 400；未声明的插件 id → 404。
//
// 与 gateway 的**刻意差异**（写进索引页提示）：
//   - 无管理鉴权（gateway 的 /ext 需要 admin 口令，这里免）；
//   - 无 /core /gw /v1 —— 插件页面里 fetch('/core/...') 在这里 404，那本来就需要完整栈；
//   - serve 子进程就是被测插件本体，参数与 gateway webproc 完全一致。
package uiproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wild-work3/ww3-conformance/internal/wire"
)

// Options 是手动联调的运行参数。
type Options struct {
	// Exe 是被测插件程序（可以是解释器）。
	Exe string
	// Args 是透传给被测进程的前缀参数（`--` 之后的参数）。
	Args []string
	// DataDir 是插件数据目录（一般由调用方建好夹具或指向真实目录）。
	DataDir string
	// ProxyAddr 是反代监听地址（必须回环；空 = 127.0.0.1:0 动态分配）。
	ProxyAddr string
	// Keep 为 true 时不清理临时目录（由调用方管理时忽略）。
	Keep bool
}

// Slot 是从 manifest.ui 解析出的一个标签。
type Slot struct {
	Pos   string // main | settings
	ID    string
	Title string
	Entry string
}

// Run 启动 serve 子进程 + /ext 反代，阻塞直到 ctx 取消（Ctrl+C）。
// 返回 nil 表示正常退出（已回收子进程），非 nil 表示启动失败或异常。
func Run(ctx context.Context, opts Options) error {
	addr := opts.ProxyAddr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if host, _, err := net.SplitHostPort(addr); err == nil && !isLoopbackHost(host) {
		return fmt.Errorf("反代地址必须回环（收到 %q）：该工具只服务本机调试", addr)
	}

	// ---- 取 manifest（不需要夹具目录：manifest 不得有副作用，与 core 同参调用）----
	m, err := manifestOf(opts)
	if err != nil {
		return err
	}
	id := str(m, "id")
	if id == "" {
		return fmt.Errorf("manifest 缺少 id 字段")
	}
	slots := parseUI(m)
	if len(slots) == 0 {
		return fmt.Errorf("manifest 未声明 ui（main/settings 都为空），没有可调试的界面")
	}

	// ---- spawn serve（与 gateway webproc 同参：--data + --listen 127.0.0.1:0）----
	argv := append(append([]string{}, opts.Args...), "serve", "--data", opts.DataDir, "--listen", "127.0.0.1:0")
	proc, err := wire.SpawnServe(ctx, id, opts.Exe, argv, 10*time.Second)
	if err != nil {
		return fmt.Errorf("拉起 serve 失败: %w", err)
	}
	target, _ := url.Parse("http://" + proc.Ready.Listen)

	// ---- 反代 ----
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		proc.Kill()
		return fmt.Errorf("监听 %s 失败: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, indexHTML(id, slots))
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/ext/") {
			notFound(w, r)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/ext/")
		pid := rest
		if i := strings.Index(rest, "/"); i >= 0 {
			pid = rest[:i]
		}
		if pid == "" || strings.ContainsAny(pid, "\\?") {
			http.Error(w, "非法插件 ID", http.StatusBadRequest)
			return
		}
		if pid != id {
			http.Error(w, "本工具只挂载被测插件 "+id+"（收到 "+pid+"）；其它插件的调试请用各自的 --exe", http.StatusNotFound)
			return
		}
		// /ext/<id>（无尾斜杠）→ 308（复刻 gateway：相对路径按目录语义解析）
		if strings.TrimPrefix(r.URL.Path, "/ext/"+id) == "" {
			http.Redirect(w, r, "/ext/"+id+"/"+r.URL.RawQuery, http.StatusPermanentRedirect)
			return
		}
		upath := strings.TrimPrefix(r.URL.Path, "/ext/"+id)
		if upath == "" {
			upath = "/"
		}
		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme = target.Scheme
				pr.Out.URL.Host = target.Host
				pr.Out.URL.Path = upath
				pr.Out.Host = target.Host
				pr.Out.Header.Set("X-WW3-Upstream", "ext:"+id) // 与 gateway 同款注入
			},
			FlushInterval: -1, // SSE 立即刷新（与 gateway 一致）
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				http.Error(w, "插件 UI 不可用: "+err.Error(), http.StatusBadGateway)
			},
			ModifyResponse: func(resp *http.Response) error {
				warnRootAbsolute(resp, id) // 自包含红线：集成前就把根绝对路径暴露出来
				return nil
			},
		}
		rp.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: mux}

	// ---- 打印访问入口 ----
	base := "http://" + ln.Addr().String()
	fmt.Fprintf(os.Stderr, "UI 联调模式（gateway 同款 /ext 反代；Ctrl+C 退出并回收插件进程）\n")
	fmt.Fprintf(os.Stderr, "  插件 serve 已就绪: %s（就绪帧: %s）\n", proc.Ready.Listen, proc.ReadyLine)
	fmt.Fprintf(os.Stderr, "  反代入口: %s/\n", base)
	for _, s := range slots {
		label := "主面板"
		if s.Pos == "settings" {
			label = "设置"
		}
		fmt.Fprintf(os.Stderr, "    [%s] %s → %s/ext/%s/%s\n", label, s.Title, base, id, s.Entry)
	}
	fmt.Fprintf(os.Stderr, "  提示：根绝对路径资源（/assets/x.js）在此会 404，与集成 gateway 后行为一致——这正是要在集成前暴露的。\n")
	fmt.Fprintf(os.Stderr, "  差异：无管理鉴权；无 /core /gw /v1（插件页面 fetch 这些前缀会 404，那需要完整栈）。\n")

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		proc.Kill()
		return fmt.Errorf("反代异常退出: %w", err)
	}

	// ---- 收尾：先停 HTTP 再杀 serve，等真退出 ----
	shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shCtx)
	proc.Kill()
	proc.Wait(5 * time.Second)
	fmt.Fprintf(os.Stderr, "已退出，插件 serve 进程已回收\n")
	return nil
}

// manifestOf 跑一次 `manifest` 并解析（无 --data：manifest 不得有副作用，与 core 调用同参）。
func manifestOf(opts Options) (map[string]any, error) {
	stdout, stderr, code, err := wire.RunOnce(context.Background(), opts.Exe,
		append(append([]string{}, opts.Args...), "manifest"), nil, nil, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("manifest 执行失败: %w（stderr: %s）", err, trunc(string(stderr), 300))
	}
	if code != 0 {
		return nil, fmt.Errorf("manifest 退出码 %d（stderr: %s）", code, trunc(string(stderr), 300))
	}
	var m map[string]any
	if err := json.Unmarshal(bodyOf(string(stdout)), &m); err != nil {
		return nil, fmt.Errorf("manifest 输出不是 JSON: %v（前 300 字节: %s）", err, trunc(string(stdout), 300))
	}
	return m, nil
}

// parseUI 解析 manifest.ui（与 suite.parseUI 同口径；uiproxy 不 import suite 以保持独立）。
func parseUI(m map[string]any) []Slot {
	raw, ok := m["ui"].(map[string]any)
	if !ok {
		return nil
	}
	var out []Slot
	for _, pos := range []string{"main", "settings"} {
		arr, _ := raw[pos].([]any)
		for _, it := range arr {
			o, ok := it.(map[string]any)
			if !ok {
				continue
			}
			s := Slot{Pos: pos}
			s.ID, _ = o["id"].(string)
			s.Title, _ = o["title"].(string)
			s.Entry, _ = o["entry"].(string)
			out = append(out, s)
		}
	}
	return out
}

// rootAbsRe / rootAbsURLRe / scanRootAbsolute 与 suite.scanRootAbsolute 同口径
// （两边各持一份而不是互相 import：suite 与 uiproxy 保持独立，手动模式不反向依赖断言层）。
var (
	rootAbsRe    = regexp.MustCompile(`(?i)(?:\bsrc|\bhref)\s*=\s*["'](/[^/"'][^"']*)["']`)
	rootAbsURLRe = regexp.MustCompile(`(?i)url\(\s*["']?(/[^/"')][^"')]*)["']?\s*\)`)
)

// scanRootAbsolute 扫出 HTML 里的根绝对路径资源引用。
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

// warnRootAbsolute 对 text/html 响应做一次性自包含扫描，命中打 stderr 警告（不改响应体）。
func warnRootAbsolute(resp *http.Response, id string) {
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		return
	}
	if resp.ContentLength > 4<<20 {
		return // 超大页面不缓冲
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return
	}
	resp.Body.Close()
	resp.Body = io.NopCloser(strings.NewReader(string(b)))
	for _, u := range scanRootAbsolute(string(b)) {
		fmt.Fprintf(os.Stderr, "  [WARN] 根绝对路径资源 %s（gateway 集成后 404；请改为相对路径）\n", u)
	}
}

// indexHTML 是反代入口的极简导航页（模拟宿主标签形态；不拷 gateway 资产）。
func indexHTML(id string, slots []Slot) string {
	var b strings.Builder
	b.WriteString("<!doctype html><meta charset='utf-8'><title>conformance UI 联调 — ")
	b.WriteString(id)
	b.WriteString("</title><body style='font:14px/1.7 system-ui,sans-serif;max-width:720px;margin:48px auto'>")
	b.WriteString("<h2>conformance UI 联调：" + id + "</h2>")
	b.WriteString("<p>以下标签经 <code>/ext/" + id + "/…</code> 反代挂载（与 gateway 集成后同路径形状）。</p><ul>")
	for _, s := range slots {
		label := "主面板"
		if s.Pos == "settings" {
			label = "设置"
		}
		fmt.Fprintf(&b, "<li>[%s] <a href='/ext/%s/%s'>%s</a> <small>(%s)</small></li>", label, id, s.Entry, s.Title, s.Entry)
	}
	b.WriteString("</ul><p><small>无管理鉴权；<code>/core</code>、<code>/gw</code>、<code>/v1</code> 不存在，需要完整栈（gateway+core）。</small></p>")
	b.WriteString("</body>")
	return b.String()
}

func notFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintf(w, "<body style='font:14px system-ui'><h3>404</h3><p>gateway 下该路径由宿主接管（/core/*、/gw/*、/v1/*）或不存在；本工具只挂载 /ext/&lt;id&gt;/*。</p></body>")
}

func isLoopbackHost(host string) bool {
	switch host {
	case "127.0.0.1", "localhost", "::1", "":
		return true
	}
	return false
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// bodyOf 取 stdout 中第一个 '{' 到最后一个 '}' 的内容（容忍日志混入）。
func bodyOf(s string) []byte {
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i < 0 || j <= i {
		return []byte(s)
	}
	return []byte(s[i : j+1])
}

// absPath 返回绝对路径（供 DataDir 校验/打印用）。
func absPath(p string) string {
	if ap, err := filepath.Abs(p); err == nil {
		return ap
	}
	return p
}
