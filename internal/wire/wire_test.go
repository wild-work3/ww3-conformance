package wire

import (
	"encoding/json"
	"testing"
)

// 帧编解码是整套准入测试的地基：出错会让所有插件同时「契约违规」。
func TestInvokeFramesParseFrames(t *testing.T) {
	raw := InvokeFrames(
		Call{Method: "ping"},
		Call{Method: "chat.generate", Params: map[string]any{"model": "m"}},
	)
	frames, err := ParseFrames(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("应得 2 帧，实得 %d", len(frames))
	}
	for i, f := range frames {
		if f.Op != OpInvoke {
			t.Fatalf("第 %d 帧 op 应为 invoke，实得 %q", i, f.Op)
		}
		if f.ID == "" {
			t.Fatalf("第 %d 帧缺 id", i)
		}
		if f.V != 1 {
			t.Fatalf("第 %d 帧 v 应为 1，实得 %d", i, f.V)
		}
	}
	if frames[0].Method != "ping" || frames[1].Method != "chat.generate" {
		t.Fatalf("方法名不匹配: %q %q", frames[0].Method, frames[1].Method)
	}
	// 第二个 invoke 必须带 params
	var p map[string]any
	if err := json.Unmarshal(frames[1].Params, &p); err != nil || p["model"] != "m" {
		t.Fatalf("params 未透传: %s (%v)", frames[1].Params, err)
	}
}

// 非 JSONL 输出（插件往 stdout 打日志）必须被识别，否则「stdout 只能出协议帧」这条契约无法验。
func TestParseFramesRejectsNonJSON(t *testing.T) {
	if _, err := ParseFrames([]byte("hello\n")); err == nil {
		t.Fatal("非 JSON 行应报错")
	}
	if _, err := ParseFrames([]byte(`{"v":1,"id":"1","op":"result","ok":true}` + "\n")); err != nil {
		t.Fatalf("合法帧不该报错: %v", err)
	}
}

func TestResultHelpers(t *testing.T) {
	r := &Result{OK: true}
	for _, e := range []Event{
		{Name: "text.delta", Data: map[string]any{"content": "你"}},
		{Name: "text.delta", Data: map[string]any{"content": "好"}},
		{Name: "reasoning.delta", Data: map[string]any{"content": "想"}},
		{Name: "finish_reason", Data: map[string]any{"reason": "stop"}},
	} {
		r.Events = append(r.Events, e)
	}
	if got := r.Text(); got != "你好" {
		t.Fatalf("Text() 应拼接 text.delta，实得 %q", got)
	}
	if got := r.Reasoning(); got != "想" {
		t.Fatalf("Reasoning() 实得 %q", got)
	}
	if got := r.Finish(); got != "stop" {
		t.Fatalf("Finish() 实得 %q", got)
	}
}
