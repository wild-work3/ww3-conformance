// hook.go 是拦截器插件（kind=hook）的通用断言（W3-51 阶段 3）。
//
// 为什么需要单独一层：`hook.stream` 是**双向流**，与渠道的「一问一答 invoke」形态完全不同，
// 渠道那套 chat.* 断言对它无意义（拦截器没有上游端点、没有模型、没有用量）。
// 现状（改造前）hook 插件零用例：`--only loopguard` 直接「没有可跑的渠道」退出码 2。
//
// 契约权威：protocol/docs/HOOK_PROTOCOL.md。本文件只写**通用**断言（不需要渠道事实），
// 插件专属的「主动终止」语义放描述文件的 hook 段（见 hookConfig）。
package suite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wild-work3/ww3-conformance/internal/check"
	"github.com/wild-work3/ww3-conformance/internal/harness"
	"github.com/wild-work3/ww3-conformance/internal/wire"
)

// hookConfig 是描述文件里 `hook` 段的形态（可选）。
//
// 只有需要验「主动终止」的插件才填：给一串要推的帧，声明期望的错误 kind。
// 例（loopguard）：
//
//	"hook": {"terminate": {"frames": [{"event":"reasoning.delta","data":{"text":"同一行…"}} …],
//	                       "want_kind": "reasoning_loop"}}
type hookConfig struct {
	Terminate *struct {
		Frames   []hookFrame `json:"frames"`
		WantKind string      `json:"want_kind"`
	} `json:"terminate"`
}

// hookFrame 是一帧（event 就是归一化事件名，data 原样）。
type hookFrame struct {
	Event string `json:"event"`
	Data  any    `json:"data,omitempty"`
}

// hookStream 跑 hook 层全部断言。
//
// 前置：manifest 已拿到（capabilities.conversation 里声明了 hook.stream）。
func hookStream(ctx context.Context, env *harness.Env, rep *check.PluginReport, cfg *hookConfig) {
	const contract = "HOOK_PROTOCOL §3"
	if !hasCap(rep.Conversation, "hook.stream") {
		rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.declared", Name: "声明 hook.stream",
			Status: check.Skip, Detail: "manifest 未声明 conversation=hook.stream（契约允许：不是拦截器插件）", Contract: contract})
		return
	}
	rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.declared", Name: "声明 hook.stream",
		Status: check.Pass, Detail: "capabilities.conversation 含 hook.stream", Contract: contract})

	// ---- hook.passthrough：普通事件原样透传 ----
	// 契约：hook 拿到一帧立刻决定转发/改写/丢弃/停止，不得整段缓存（§3.2.1）。
	push := []hookFrame{
		{Event: "text.delta", Data: map[string]any{"text": "第一段"}},
		{Event: "text.delta", Data: map[string]any{"text": "第二段"}},
		{Event: "text.delta", Data: map[string]any{"text": "第三段"}},
	}
	got, err := runHookStream(ctx, env, push, "upstream.end", map[string]any{"ok": true})
	if err != nil {
		rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.stream", Name: "hook.stream 可调用",
			Status: check.Fail, Detail: err.Error(), Contract: contract})
		return
	}
	rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.stream", Name: "hook.stream 可调用",
		Status: check.Pass, Detail: fmt.Sprintf("收到 %d 帧，result ok=%v", len(got.Events), got.OK), Contract: contract})

	texts := deltaTexts(got.Events)
	want := []string{"第一段", "第二段", "第三段"}
	if sameStrings(texts, want) {
		rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.passthrough", Name: "普通事件原样透传",
			Status: check.Pass, Detail: "3 帧 text.delta 逐帧转发，顺序与内容不变", Contract: contract})
	} else {
		rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.passthrough", Name: "普通事件原样透传",
			Status: check.Fail, Detail: fmt.Sprintf("期望 %v，实际 %v", want, texts), Contract: contract})
	}

	// ---- hook.end：upstream.end 不转发下游 ----
	if hasEvent(got.Events, "upstream.end") {
		rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.end", Name: "upstream.end 不转发",
			Status: check.Fail, Detail: "下游收到了 upstream.end（该帧是 core 与 hook 之间的结束信号，不得转发）", Contract: contract})
	} else if got.OK {
		rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.end", Name: "upstream.end 不转发",
			Status: check.Pass, Detail: "未转发 upstream.end，且 result ok=true", Contract: contract})
	} else {
		rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.end", Name: "upstream.end 不转发",
			Status: check.Fail, Detail: "upstream.end{ok:true} 之后 result 不是 ok=true: "+errKindText(got.Err), Contract: contract})
	}

	// ---- hook.no-error-event：不得自行发 error 终止帧 ----
	// 契约 §2-Y 红线：终止事件只能出现一次，由 core 统一发（hook 返回错误即可）。
	if hasEvent(got.Events, "error") {
		rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.no-error-event", Name: "不自行发 error 终止帧",
			Status: check.Fail, Detail: "hook 自己发了 event=error（应 return 错误，由 core 统一发终止事件）", Contract: contract})
	} else {
		rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.no-error-event", Name: "不自行发 error 终止帧",
			Status: check.Pass, Detail: "全帧扫描无 event=error", Contract: contract})
	}

	// ---- hook.no-cache：不做整段缓存（首帧延迟不随帧数增长）----
	// 判据是「首帧透传延迟」：契约要求边收边转（§3.2.1 硬约束 1）。
	// 阈值取 300ms —— 留足进程调度余量，但整段缓存（要等全部帧推完）会远超它。
	if d, ok := firstEventDelay(ctx, env, rep); ok {
		if d < 300*time.Millisecond {
			rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.no-cache", Name: "不做整段缓存",
				Status: check.Pass, Detail: fmt.Sprintf("首帧透传延迟 %dms（<300ms）", d.Milliseconds()), Contract: contract})
		} else {
			rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.no-cache", Name: "不做整段缓存",
				Status: check.Fail, Detail: fmt.Sprintf("首帧透传延迟 %dms（>=300ms，疑似整段缓存后才转发）", d.Milliseconds()), Contract: contract})
		}
	}

	// ---- hook.terminate：主动终止（可选，由描述文件声明）----
	if cfg != nil && cfg.Terminate != nil && len(cfg.Terminate.Frames) > 0 {
		hookTerminate(ctx, env, rep, cfg.Terminate.Frames, cfg.Terminate.WantKind)
	}
}

// hookTerminate 验「命中即终止」语义：hook 返回错误而不是正常收尾。
func hookTerminate(ctx context.Context, env *harness.Env, rep *check.PluginReport, frames []hookFrame, wantKind string) {
	const contract = "HOOK_PROTOCOL §3.4"
	got, err := runHookStream(ctx, env, frames, "", nil)
	if err != nil {
		rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.terminate", Name: "命中即终止",
			Status: check.Fail, Detail: err.Error(), Contract: contract})
		return
	}
	if got.OK {
		rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.terminate", Name: "命中即终止",
			Status: check.Fail, Detail: "推完触发帧后 result 仍 ok=true（应返回错误终止请求）", Contract: contract})
		return
	}
	kind := ""
	if got.Err != nil {
		kind = got.Err.Kind
	}
	if wantKind != "" && kind != wantKind {
		rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.terminate", Name: "命中即终止",
			Status: check.Fail, Detail: fmt.Sprintf("错误 kind=%q（期望 %q）", kind, wantKind), Contract: contract})
		return
	}
	rep.Add(check.Assertion{Level: check.LevelHook, ID: "hook.terminate", Name: "命中即终止",
		Status: check.Pass, Detail: "命中后返回错误 kind=" + kind + "（由 core 统一发终止事件）", Contract: contract})
}

// runHookStream 拉起 stdio 进程、调用 hook.stream、按顺序推帧、可选的结束帧，然后等 result。
//
// endEvent 非空时在推完 frames 后追加一帧（通常是 upstream.end）。
func runHookStream(ctx context.Context, env *harness.Env, frames []hookFrame, endEvent string, endData any) (*wire.Result, error) {
	cctx, cancel := context.WithTimeout(ctx, harness.DefaultTimeout)
	defer cancel()
	c, err := env.Stdio(cctx)
	if err != nil {
		return nil, fmt.Errorf("拉起 stdio 进程失败: %w", err)
	}
	defer c.Close()

	done := make(chan *wire.Result, 1)
	var callErr error
	var id string
	go func() {
		res, e := c.Call(cctx, wire.Call{
			Method: "hook.stream",
			Params: map[string]any{},
			OnPush: func(raw json.RawMessage) {
				// hook 主动推帧（hook→下游方向）；这里只记录，断言在 result 之后统一做
			},
		})
		callErr = e
		done <- res
	}()
	// 等 invoke 帧真正写出去（Call 内部会分配 id；这里用短轮询取当前 id）
	for i := 0; i < 200; i++ {
		if v := c.LastID(); v != "" {
			id = v
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if id == "" {
		return nil, fmt.Errorf("未能取得 invoke 的请求 ID")
	}
	for _, f := range frames {
		if err := c.PushEvent(id, f.Event, f.Data); err != nil {
			return nil, fmt.Errorf("推帧 %s 失败: %w", f.Event, err)
		}
	}
	if endEvent != "" {
		if err := c.PushEvent(id, endEvent, endData); err != nil {
			return nil, fmt.Errorf("推结束帧失败: %w", err)
		}
	}
	select {
	case <-cctx.Done():
		return nil, fmt.Errorf("hook.stream 超时/取消: %w", cctx.Err())
	case res := <-done:
		// 注意：result ok=false 是**合法结果**（hook 主动终止就是这条路径），
		// wire.Call 会把它同时当 error 返回。只有「拿不到 result」才算调用失败。
		if callErr != nil && (res == nil || (res.Err == nil && res.Raw == nil)) {
			return nil, callErr
		}
		if res == nil {
			return nil, callErr
		}
		return res, nil
	}
}

// firstEventDelay 量「推第一帧 → 收到第一帧转发」的延迟。
//
// 只推 1 帧然后立刻推 upstream.end：整段缓存的实现会等到结束才转发，
// 于是这一帧的转发时间戳会落在结束之后（延迟显著偏大）。
func firstEventDelay(ctx context.Context, env *harness.Env, rep *check.PluginReport) (time.Duration, bool) {
	cctx, cancel := context.WithTimeout(ctx, harness.DefaultTimeout)
	defer cancel()
	c, err := env.Stdio(cctx)
	if err != nil {
		return 0, false
	}
	defer c.Close()
	var firstAt time.Time
	sentAt := time.Time{}
	done := make(chan *wire.Result, 1)
	go func() {
		res, _ := c.Call(cctx, wire.Call{
			Method: "hook.stream",
			Params: map[string]any{},
			OnEvent: func(name string, raw json.RawMessage) {
				if firstAt.IsZero() && name != "upstream.end" {
					firstAt = time.Now()
				}
			},
			OnPush: func(raw json.RawMessage) {
				if firstAt.IsZero() {
					firstAt = time.Now()
				}
			},
		})
		done <- res
	}()
	id := waitID(c)
	if id == "" {
		return 0, false
	}
	sentAt = time.Now()
	if err := c.PushEvent(id, "text.delta", map[string]any{"text": "首帧"}); err != nil {
		return 0, false
	}
	if err := c.PushEvent(id, "upstream.end", map[string]any{"ok": true}); err != nil {
		return 0, false
	}
	select {
	case <-cctx.Done():
		return 0, false
	case <-done:
	}
	if firstAt.IsZero() {
		return 0, false
	}
	return firstAt.Sub(sentAt), true
}

func waitID(c *wire.Client) string {
	for i := 0; i < 200; i++ {
		if v := c.LastID(); v != "" {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	return ""
}

// deltaTexts 取所有 text.delta 帧的正文。
func deltaTexts(evs []wire.Event) []string {
	var out []string
	for _, e := range evs {
		if e.Name != "text.delta" {
			continue
		}
		if s, ok := e.Data["text"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func hasEvent(evs []wire.Event, name string) bool {
	for _, e := range evs {
		if e.Name == name {
			return true
		}
	}
	return false
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimSpace(a[i]) != strings.TrimSpace(b[i]) {
			return false
		}
	}
	return true
}

func errKindText(e *wire.ErrObj) string {
	if e == nil {
		return "(无错误对象)"
	}
	return e.Kind + ": " + e.Message
}
