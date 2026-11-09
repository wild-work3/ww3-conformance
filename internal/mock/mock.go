// Package mock 提供可编程的本地 mock 上游：所有请求都记录，响应由路由表决定。
//
// 设计目的：准入测试必须离线可跑（真实上游只用于 flash 模型抽查），
// 而各渠道上游协议差异极大（OpenAI/Anthropic/v1internal/私有信封/签名），
// 因此这里不做「通用假上游」，而是「路由表 + 少量 SSE 适配器」：
//   - 路由表按 方法+路径正则 匹配，返回 JSON / 各类 SSE / 原始字节
//   - 所有请求（含 headers、body）落 request log，供断言「插件实际发了什么」
//   - 未命中路由一律 404 + 记录，便于探测插件真实请求形状
package mock

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Kind 是响应类型。
type Kind string

const (
	KindJSON         Kind = "json"          // 直接回 Body（JSON）
	KindRaw          Kind = "raw"           // 直接回 Body（原始字符串）
	KindSSEOpenAI    Kind = "sse_openai"    // OpenAI chat.completion.chunk 流
	KindSSEAnthropic Kind = "sse_anthropic" // Anthropic Messages 事件流
	KindSSENested    Kind = "sse_nested"    // 嵌套信封（外层 data 的 body 是内层 chunk JSON 字符串）
	KindSSEText      Kind = "sse_text"      // 通用：每行 `data: <json>`，内容由 Text 指定
	KindSSERaw       Kind = "sse_raw"       // Body 是多行 SSE 原文，逐行原样发出
	KindStatus       Kind = "status"        // 只回状态码（错误场景）
)

// Route 是一条路由规则。
type Route struct {
	Method string // 空=任意
	// Path 是正则（对 URL.Path，不含 query）。
	Path string
	Kind Kind
	// Status 非 0 时覆盖默认 200。
	Status int
	// Body 用于 KindJSON / KindRaw / KindStatus / KindSSERaw（多行 SSE 原文）。
	Body string
	// Text 用于 KindSSEText：作为唯一一段正文。
	Text string
	// Delay 是响应前等待（模拟慢上游，测 idle 看门狗）。
	Delay time.Duration
	// Chunks 用于 KindSSEOpenAI / KindSSEAnthropic：自定义分片数（默认按 Text 切两片）。
	Chunks int
	// Usage 用于 SSE 适配器：是否在末尾带 usage 帧。
	Usage *Usage
	// NoUsage 显式声明「上游不报 usage」（不带 usage 帧）。
	NoUsage bool
}

// Usage 是 mock 上报的用量。
type Usage struct {
	Prompt     int
	Completion int
	Cached     int
	Write      int
}

// Req 是一条被记录的请求。
type Req struct {
	At      time.Time
	Method  string
	Path    string
	Query   string
	Headers map[string][]string
	Body    string
	// Matched 是命中的路由 Path 正则（未命中为空）。
	Matched string
	Status  int
}

// Server 是 mock 上游。
type Server struct {
	mu     sync.Mutex
	routes []compiled
	reqs   []Req
	srv    *http.Server
	ln     net.Listener
	// Base 是 http://127.0.0.1:<port>
	Base string
	// DefaultText 是 SSE 适配器缺省正文。
	DefaultText string
}

type compiled struct {
	Route
	re *regexp.Regexp
}

// New 创建 mock（不启动）。
func New(routes []Route) (*Server, error) {
	s := &Server{DefaultText: "你好，我是 mock 上游。"}
	for _, r := range routes {
		re, err := regexp.Compile(r.Path)
		if err != nil {
			return nil, fmt.Errorf("路由正则非法 %q: %w", r.Path, err)
		}
		s.routes = append(s.routes, compiled{Route: r, re: re})
	}
	return s, nil
}

// Start 监听 127.0.0.1 的随机端口。
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.ln = ln
	s.Base = "http://" + ln.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.srv = &http.Server{Handler: mux}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

// AddRoute 动态追加路由（探测阶段用）。
func (s *Server) AddRoute(r Route) error {
	re, err := regexp.Compile(r.Path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes = append(s.routes, compiled{Route: r, re: re})
	return nil
}

// SetRoutes 替换全部路由。
func (s *Server) SetRoutes(rs []Route) error {
	var out []compiled
	for _, r := range rs {
		re, err := regexp.Compile(r.Path)
		if err != nil {
			return err
		}
		out = append(out, compiled{Route: r, re: re})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes = out
	return nil
}

// Close 停止。
func (s *Server) Close() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
}

// Requests 返回请求记录快照。
func (s *Server) Requests() []Req {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Req, len(s.reqs))
	copy(out, s.reqs)
	return out
}

// Reset 清空请求记录。
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = nil
}

// Find 返回第一个满足 method+path 子串匹配的请求。
func (s *Server) Find(method, pathSub string) *Req {
	for _, r := range s.Requests() {
		if (method == "" || r.Method == method) && strings.Contains(r.Path, pathSub) {
			return &r
		}
	}
	return nil
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	rec := Req{At: time.Now(), Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		Headers: map[string][]string{}, Body: string(body)}
	for k, v := range r.Header {
		rec.Headers[k] = v
	}
	var hit *compiled
	s.mu.Lock()
	for i := range s.routes {
		rt := &s.routes[i]
		if rt.Method != "" && !strings.EqualFold(rt.Method, r.Method) {
			continue
		}
		if rt.re.MatchString(r.URL.Path) {
			hit = rt
			break
		}
	}
	s.mu.Unlock()

	if hit == nil {
		rec.Status = 404
		s.record(rec)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"message":"mock: no route for ` + r.Method + ` ` + r.URL.Path + `"}}`))
		return
	}
	rec.Matched = hit.Path
	if hit.Delay > 0 {
		time.Sleep(hit.Delay)
	}
	status := hit.Status
	if status == 0 {
		status = 200
	}
	rec.Status = status
	s.record(rec)

	switch hit.Kind {
	case KindJSON:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(hit.Body))
	case KindRaw:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(hit.Body))
	case KindStatus:
		if hit.Body != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		if hit.Body != "" {
			_, _ = w.Write([]byte(hit.Body))
		}
	case KindSSEOpenAI:
		s.sseOpenAI(w, hit, r)
	case KindSSEAnthropic:
		s.sseAnthropic(w, hit, r)
	case KindSSENested:
		s.sseNested(w, hit)
	case KindSSEText:
		s.sseText(w, hit)
	case KindSSERaw:
		s.sseRaw(w, hit)
	default:
		w.WriteHeader(status)
	}
}

func (s *Server) record(r Req) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) < 5000 {
		s.reqs = append(s.reqs, r)
	}
}

func (s *Server) textOf(rt *compiled) string {
	if rt.Text != "" {
		return rt.Text
	}
	return s.DefaultText
}

func (s *Server) sseText(w http.ResponseWriter, rt *compiled) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", s.textOf(rt))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// sseRaw 把 Body 里的每一行作为一条 SSE 帧原样发出（Body 是多行文本）。
//
// 用于私有信封/命名事件类上游（qoder 嵌套信封、glm assistant 帧、traework SOLO 命名事件）：
//   - 行以 "event:" / "data:" / ":" 开头时整条原样写出（已是完整帧）
//   - 空行只作分隔（不重复发）
//   - 其余行自动加 "data: " 前缀
//
// 之所以用「多行文本」而不是 JSON 数组：命名事件流需要 `event:` 与 `data:` 两个独立行，
// JSON 数组里每项只能是一行，无法表达 `event:\ndata:\n\n` 这种帧。
func (s *Server) sseRaw(w http.ResponseWriter, rt *compiled) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	// skipUsage 用于「上游不报 usage」场景（harness.UseNoUsage）：
	// KindSSERaw 的帧是预烘的原文，改 Route.Usage 无效，只能在这里剥掉用量帧。
	// 判定依据是帧载荷里的用量键（usage / usageMetadata / token_usage 事件 / *_tokens），
	// 命名事件流要先看到 `event: token_usage` 才能连带跳过它下面那行 data。
	skipNextData := false
	for _, it := range strings.Split(strings.ReplaceAll(rt.Body, "\r\n", "\n"), "\n") {
		it = strings.TrimRight(it, "\r")
		// 空行是 SSE 的帧分隔符，**必须原样发出**：命名事件流（event:/data: 两行 + 空行）
		// 靠它切帧，丢弃空行会让所有帧挤在一个 data 缓冲里（traework 正文全空就是这么来的）。
		if strings.TrimSpace(it) == "" {
			_, _ = fmt.Fprint(w, "\n")
			if fl != nil {
				fl.Flush()
			}
			continue
		}
		if rt.NoUsage {
			if strings.HasPrefix(it, "event:") && strings.Contains(it, "token_usage") {
				skipNextData = true
				continue
			}
			if skipNextData && strings.HasPrefix(it, "data:") {
				skipNextData = false
				continue
			}
			if strings.HasPrefix(it, "data:") && hasUsagePayload(it) {
				continue
			}
		}
		switch {
		case strings.HasPrefix(it, "event:"), strings.HasPrefix(it, "data:"), strings.HasPrefix(it, ":"):
			_, _ = fmt.Fprintf(w, "%s\n", it)
		default:
			_, _ = fmt.Fprintf(w, "data: %s\n", it)
		}
		if fl != nil {
			fl.Flush()
		}
	}
}

// sseOpenAI 输出 OpenAI chat.completion.chunk 流（含首字/正文/结束/可选 usage）。
func (s *Server) sseOpenAI(w http.ResponseWriter, rt *compiled, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	model := "mock-model"
	write := func(obj any) {
		b, _ := json.Marshal(obj)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	base := func() map[string]any {
		return map[string]any{"id": "cmpl-mock", "object": "chat.completion.chunk", "model": model}
	}
	txt := s.textOf(rt)
	n := rt.Chunks
	if n <= 0 {
		n = 2
	}
	parts := splitN(txt, n)
	for _, p := range parts {
		f := base()
		f["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"content": p}}}
		write(f)
	}
	f := base()
	f["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}
	write(f)
	if !rt.NoUsage {
		u := rt.Usage
		if u == nil {
			u = &Usage{Prompt: 11, Completion: 7}
		}
		uw := map[string]any{"prompt_tokens": u.Prompt, "completion_tokens": u.Completion,
			"total_tokens": u.Prompt + u.Completion}
		if u.Cached > 0 {
			uw["prompt_tokens_details"] = map[string]any{"cached_tokens": u.Cached}
		}
		if u.Write > 0 {
			uw["cache_creation_input_tokens"] = u.Write
		}
		write(map[string]any{"id": "cmpl-mock", "object": "chat.completion.chunk", "model": model,
			"choices": []any{}, "usage": uw})
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if fl != nil {
		fl.Flush()
	}
}

// sseAnthropic 输出 Anthropic Messages 事件流。
func (s *Server) sseAnthropic(w http.ResponseWriter, rt *compiled, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	ev := func(name string, obj any) {
		b, _ := json.Marshal(obj)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		if fl != nil {
			fl.Flush()
		}
	}
	// NoUsage 时连 message_start 里的 usage 也不能带：Anthropic 客户端会把
	// message_start.usage 当作上游已报的用量（minimax/monkeycode/zcode 实测如此）。
	//
	// input/cache 三维必须在 message_start 就给（Anthropic 语义：input 只在 start 报，
	// message_delta 只报 output）。此处不随 Route.Usage 变会让夹具永远测不到缓存转发
	// ——monkeycode 实测报 prompt=9（写死的默认值）而不是上游的 13。
	startIn, startCached, startWrite := 9, 0, 0
	if rt.Usage != nil {
		startIn, startCached, startWrite = rt.Usage.Prompt, rt.Usage.Cached, rt.Usage.Write
	}
	startUsage := any(nil)
	if !rt.NoUsage {
		su := map[string]any{"input_tokens": startIn, "output_tokens": 1}
		if startCached > 0 {
			su["cache_read_input_tokens"] = startCached
		}
		if startWrite > 0 {
			su["cache_creation_input_tokens"] = startWrite
		}
		startUsage = su
	}
	ev("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_mock", "type": "message", "role": "assistant", "model": "mock-model",
		"content": []any{}, "stop_reason": nil, "usage": startUsage}})
	ev("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""}})
	txt := s.textOf(rt)
	n := rt.Chunks
	if n <= 0 {
		n = 2
	}
	for _, p := range splitN(txt, n) {
		ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": p}})
	}
	ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	u := map[string]any{"input_tokens": 9, "output_tokens": 5}
	if rt.Usage != nil {
		u["input_tokens"] = rt.Usage.Prompt
		u["output_tokens"] = rt.Usage.Completion
		if rt.Usage.Cached > 0 {
			u["cache_read_input_tokens"] = rt.Usage.Cached
		}
		if rt.Usage.Write > 0 {
			u["cache_creation_input_tokens"] = rt.Usage.Write
		}
	}
	delta := map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}}
	if !rt.NoUsage {
		delta["usage"] = u
	}
	ev("message_delta", delta)
	ev("message_stop", map[string]any{"type": "message_stop"})
	if fl != nil {
		fl.Flush()
	}
}

// sseNested 输出 qoder 式嵌套信封 SSE：每行 `data:` 的 body 字段是**内层 chunk 的 JSON 字符串**。
//
// 这是 qoder/qwenwork 类上游的真实形态，不能用 sseOpenAI 代替（插件解外层信封会失败）。
func (s *Server) sseNested(w http.ResponseWriter, rt *compiled) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	inner := func(obj any) {
		b, _ := json.Marshal(obj)
		env := map[string]any{"body": string(b), "headers": map[string]any{}, "statusCodeValue": 200}
		eb, _ := json.Marshal(env)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", eb)
		if fl != nil {
			fl.Flush()
		}
	}
	txt := s.textOf(rt)
	n := rt.Chunks
	if n <= 0 {
		n = 2
	}
	for _, p := range splitN(txt, n) {
		inner(map[string]any{"id": "qoder-mock", "object": "chat.completion.chunk", "model": "mock-model",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": p}}}})
	}
	if !rt.NoUsage {
		u := rt.Usage
		if u == nil {
			u = &Usage{Prompt: 11, Completion: 7}
		}
		uw := map[string]any{"prompt_tokens": u.Prompt, "completion_tokens": u.Completion,
			"total_tokens": u.Prompt + u.Completion}
		if u.Cached > 0 {
			uw["prompt_tokens_details"] = map[string]any{"cached_tokens": u.Cached}
		}
		inner(map[string]any{"id": "qoder-mock", "object": "chat.completion.chunk", "model": "mock-model",
			"choices": []any{}, "usage": uw})
	}
	inner(map[string]any{"id": "qoder-mock", "object": "chat.completion.chunk", "model": "mock-model",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
}

// splitN 把文本切成 n 段（n<=1 时整段）。
func splitN(s string, n int) []string {
	if n <= 1 {
		return []string{s}
	}
	r := []rune(s)
	if len(r) <= n {
		out := make([]string, 0, len(r))
		for _, c := range r {
			out = append(out, string(c))
		}
		return out
	}
	step := len(r) / n
	var out []string
	for i := 0; i < n; i++ {
		lo := i * step
		hi := lo + step
		if i == n-1 {
			hi = len(r)
		}
		out = append(out, string(r[lo:hi]))
	}
	return out
}

// hasUsagePayload 判断一帧 data: 行是否携带用量信息。
//
// 用于 NoUsage 场景剥离 KindSSERaw 预烘帧里的用量（Body 是原文，改 Route.Usage 无效）。
// 只看键名不看值：`"usage":null` 也算（上游显式声明无用量）。
func hasUsagePayload(line string) bool {
	// 嵌套信封（qoder/qwenwork）把内层 chunk 作为**字符串**塞进 body 字段，
	// 引号是转义态（`\"usage\"`），不反转义就永远匹配不到（实测 qwenwork 的
	// usage.absent 因此误判「臆造用量」）。
	line = strings.ReplaceAll(line, `\"`, `"`)
	for _, k := range []string{`"usage"`, `"usageMetadata"`, `"usage_metadata"`, `"token_usage"`, `"prompt_tokens"`, `"PromptTokens"`} {
		if strings.Contains(line, k) {
			return true
		}
	}
	return false
}
