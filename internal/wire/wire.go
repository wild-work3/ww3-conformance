// Package wire 是最小 JSONL RPC 线协议实现（W3-28 路②：按文本契约自实现，不复制 main/protocol）。
//
// 准入测试的父进程扮演 core 的角色：spawn 插件、下发 invoke、收 event/result。
// 只实现测试需要的那部分语义（invoke/event/result/push + 超时 + 强杀），
// 契约权威仍是 protocol/docs/PLUGIN_PROTOCOL.md 与 protocol/docs/IPC_CHAT.md。
package wire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// 帧操作（PLUGIN_PROTOCOL §3）。
const (
	OpInvoke = "invoke"
	OpEvent  = "event"
	OpResult = "result"
	OpPush   = "push"
)

// MaxFrame 单帧上限，与 main/protocol/jsonl 的 Decoder 保持一致（64MB）。
const MaxFrame = 64 << 20

// Frame 是一行 JSONL。
type Frame struct {
	V      int             `json:"v,omitempty"`
	ID     string          `json:"id,omitempty"`
	Op     string          `json:"op,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Event  string          `json:"event,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	OK     *bool           `json:"ok,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ErrObj         `json:"error,omitempty"`
}

// ErrObj 是统一错误结构（PLUGIN_PROTOCOL §6 / errkind）。
type ErrObj struct {
	Kind         string `json:"kind"`
	Scope        string `json:"scope"`
	Message      string `json:"message"`
	Retryable    *bool  `json:"retryable,omitempty"`
	RetryAfterMs int64  `json:"retry_after_ms,omitempty"`
	Cause        string `json:"cause,omitempty"`
}

func (e *ErrObj) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("[%s]%s: %s", e.Kind, e.Scope, e.Message)
}

// Call 是一次调用的输入。
type Call struct {
	Method  string
	Params  any
	OnEvent func(ev string, data json.RawMessage)
	// OnPush 处理对端主动推来的 push 帧（hook.stream 的请求方向；渠道插件不用）。
	OnPush func(data json.RawMessage)
}

// Result 是一次调用的输出。
type Result struct {
	Method string
	OK     bool
	Raw    json.RawMessage // result 原始 JSON（标量/对象/数组都可能）
	Obj    map[string]any  // result 为对象时的解析结果，否则 nil
	Events []Event
	Err    *ErrObj
	Took   time.Duration
	Stderr string
}

// Event 是一条事件帧。
type Event struct {
	Name string
	Data map[string]any
	Raw  json.RawMessage
}

// Text 拼接全部 text.delta。
func (r *Result) Text() string {
	var b strings.Builder
	for _, e := range r.Events {
		if e.Name == "text.delta" {
			if s, ok := e.Data["content"].(string); ok {
				b.WriteString(s)
			}
		}
	}
	return b.String()
}

// Reasoning 拼接全部 reasoning.delta。
func (r *Result) Reasoning() string {
	var b strings.Builder
	for _, e := range r.Events {
		if e.Name == "reasoning.delta" {
			if s, ok := e.Data["content"].(string); ok {
				b.WriteString(s)
			}
		}
	}
	return b.String()
}

// Finish 取 finish_reason 事件的 reason（取最后一个）。
func (r *Result) Finish() string {
	out := ""
	for _, e := range r.Events {
		if e.Name == "finish_reason" {
			if s, ok := e.Data["reason"].(string); ok {
				out = s
			}
		}
	}
	return out
}

// UsageEvent 取最后一帧 usage 事件数据。
func (r *Result) UsageEvent() map[string]any {
	var out map[string]any
	for _, e := range r.Events {
		if e.Name == "usage" {
			out = e.Data
		}
	}
	return out
}

// EventNames 返回事件名序列（判顺序/唯一性用）。
func (r *Result) EventNames() []string {
	out := make([]string, 0, len(r.Events))
	for _, e := range r.Events {
		out = append(out, e.Name)
	}
	return out
}

// Client 是长驻 stdio 子进程的 RPC 客户端。
type Client struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	name    string
	exe     string
	args    []string
	encMu   sync.Mutex
	pending map[string]chan Frame
	mu      sync.Mutex
	stderr  *syncBuffer
	closed  bool
	readErr error
	seq     int
	done    chan struct{}
}

// syncBuffer 是并发安全的 stderr 收集器。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.b.Len() < 1<<20 { // 最多留 1MB，避免插件刷日志吃内存
		s.b.Write(p)
	}
	return len(p), nil
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Spawn 启动插件的 stdio 形态并开始读循环。
func Spawn(ctx context.Context, name, exe string, args []string, extraEnv []string) (*Client, error) {
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("打开 stdin 失败: %w", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("打开 stdout 失败: %w", err)
	}
	sb := &syncBuffer{}
	cmd.Stderr = sb
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 %s 失败: %w", name, err)
	}
	c := &Client{cmd: cmd, stdin: in, name: name, exe: exe, args: args,
		pending: map[string]chan Frame{}, stderr: sb, done: make(chan struct{})}
	go c.readLoop(out)
	go func() {
		select {
		case <-ctx.Done():
			c.Kill()
		case <-c.done:
		}
	}()
	return c, nil
}

func (c *Client) readLoop(out io.Reader) {
	defer close(c.done)
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 1<<20), MaxFrame)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			c.mu.Lock()
			if c.readErr == nil {
				c.readErr = fmt.Errorf("非 JSONL 输出（stdout 只能出协议帧）: %s", truncate(string(line), 300))
			}
			c.mu.Unlock()
			continue
		}
		c.mu.Lock()
		ch := c.pending[f.ID]
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- f:
			case <-c.done:
				return
			}
		}
	}
	c.mu.Lock()
	if c.readErr == nil {
		if err := sc.Err(); err != nil {
			c.readErr = err
		}
	}
	c.mu.Unlock()
	// 进程退出：唤醒所有等待者（用零值帧表示 EOF）
	c.mu.Lock()
	for id, ch := range c.pending {
		select {
		case ch <- Frame{V: 0}:
		default:
		}
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

// Call 下发一次 invoke 并等待 result。
func (c *Client) Call(ctx context.Context, call Call) (*Result, error) {
	res := &Result{Method: call.Method}
	id := c.nextID()
	ch := make(chan Frame, 256)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	params, err := json.Marshal(call.Params)
	if err != nil {
		return res, fmt.Errorf("序列化 params 失败: %w", err)
	}
	if call.Params == nil {
		params = nil
	}
	f := Frame{V: 1, ID: id, Op: OpInvoke, Method: call.Method, Params: params}
	start := time.Now()
	if err := c.send(f); err != nil {
		return res, fmt.Errorf("写帧失败: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			res.Took = time.Since(start)
			res.Stderr = c.Stderr()
			return res, fmt.Errorf("调用 %s 超时/取消: %w", call.Method, ctx.Err())
		case <-c.done:
			res.Took = time.Since(start)
			res.Stderr = c.Stderr()
			c.mu.Lock()
			re := c.readErr
			c.mu.Unlock()
			if re != nil {
				return res, re
			}
			return res, errors.New("子进程在返回 result 前退出")
		case fr := <-ch:
			if fr.V == 0 && fr.Op == "" {
				res.Took = time.Since(start)
				res.Stderr = c.Stderr()
				return res, errors.New("子进程在返回 result 前退出")
			}
			switch fr.Op {
			case OpEvent:
				ev := Event{Name: fr.Event, Raw: fr.Data}
				if len(fr.Data) > 0 {
					_ = json.Unmarshal(fr.Data, &ev.Data)
				}
				res.Events = append(res.Events, ev)
				if call.OnEvent != nil {
					call.OnEvent(fr.Event, fr.Data)
				}
			case OpPush:
				if call.OnPush != nil {
					call.OnPush(fr.Data)
				}
			case OpResult:
				res.Took = time.Since(start)
				res.Stderr = c.Stderr()
				ok := fr.OK != nil && *fr.OK
				res.OK = ok
				res.Raw = fr.Result
				if len(fr.Result) > 0 {
					_ = json.Unmarshal(fr.Result, &res.Obj)
				}
				if !ok {
					res.Err = fr.Error
					if res.Err == nil {
						res.Err = &ErrObj{Kind: "unknown", Scope: "request", Message: "ok=false 但无 error"}
					}
					return res, res.Err
				}
				return res, nil
			}
		}
	}
}

// Push 向对端发 push 帧（双向流用）。data 作为帧的 data 字段原样序列化。
func (c *Client) Push(id string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return c.send(Frame{V: 1, ID: id, Op: OpPush, Data: b})
}

// PushEvent 发一帧带 event 名的 push（hook.stream 的上游→hook 方向）。
//
// 契约（HOOK_PROTOCOL §3.2）：帧的 `event` 字段直接就是归一化事件名，`data` 原样。
func (c *Client) PushEvent(id, event string, data any) error {
	var b []byte
	if data != nil {
		var err error
		if b, err = json.Marshal(data); err != nil {
			return err
		}
	}
	return c.send(Frame{V: 1, ID: id, Op: OpPush, Event: event, Data: b})
}

// LastID 返回最近一次分配出去的请求 ID（双向流场景下调用方需要它才能回推帧）。
//
// 注意：它是「最近一个」而不是「某一个」——双向流必须保证一次只有一个在途 invoke，
// 否则无法区分该往哪个 id 推。准入测试正是这个用法（一次一调）。
func (c *Client) LastID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seq == 0 {
		return ""
	}
	return fmt.Sprintf("cf%d", c.seq)
}

func (c *Client) send(f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.encMu.Lock()
	defer c.encMu.Unlock()
	_, err = c.stdin.Write(b)
	return err
}

func (c *Client) nextID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	return fmt.Sprintf("cf%d", c.seq)
}

// Stderr 返回至今收集到的 stderr。
func (c *Client) Stderr() string { return c.stderr.String() }

// Pid 返回子进程 pid。
func (c *Client) Pid() int {
	if c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// Close 关闭 stdin（触发插件 EOF 退出）并回收。
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	_ = c.stdin.Close()
	wait := make(chan error, 1)
	go func() { wait <- c.cmd.Wait() }()
	select {
	case err := <-wait:
		return err
	case <-time.After(5 * time.Second):
		c.Kill()
		return <-wait
	}
}

// Kill 强杀子进程。
func (c *Client) Kill() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

// RunOnce 跑一次一次性进程：stdin 写若干帧，收集 stdout 全部帧，返回退出码。
// 用于 call 形态与 CLI 便利命令（stdout 非 JSONL 的场合由调用方自己解析）。
func RunOnce(ctx context.Context, exe string, args []string, stdin []byte, env []string, timeout time.Duration) (stdout, stderr []byte, code int, err error) {
	cctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(cctx, exe, args...)
	cmd.Env = append(os.Environ(), env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var ob, eb bytes.Buffer
	cmd.Stdout = &ob
	cmd.Stderr = &eb
	err = cmd.Run()
	code = 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
			err = nil
		} else if cctx.Err() != nil {
			return ob.Bytes(), eb.Bytes(), -1, fmt.Errorf("超时/取消: %w", cctx.Err())
		} else {
			return ob.Bytes(), eb.Bytes(), -1, err
		}
	}
	return ob.Bytes(), eb.Bytes(), code, nil
}

// InvokeFrames 拼一条 invoke 帧（一次性进程的 stdin）。
func InvokeFrames(calls ...Call) []byte {
	var b bytes.Buffer
	for i, c := range calls {
		p, _ := json.Marshal(c.Params)
		if c.Params == nil {
			p = nil
		}
		f := Frame{V: 1, ID: fmt.Sprintf("1%d", i), Op: OpInvoke, Method: c.Method, Params: p}
		line, _ := json.Marshal(f)
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// ParseFrames 解析一次性进程的 stdout 帧。
func ParseFrames(stdout []byte) ([]Frame, error) {
	var out []Frame
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 1<<20), MaxFrame)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			return out, fmt.Errorf("非 JSONL 行: %s", truncate(string(line), 200))
		}
		out = append(out, f)
	}
	return out, sc.Err()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
