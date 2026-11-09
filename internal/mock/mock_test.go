package mock

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// 对话类路由都注册成 POST，夹具必须用 POST 打，否则会落到「未命中 404」。
func post(t *testing.T, base, path string) string {
	t.Helper()
	resp, err := http.Post(base+path, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// KindSSERaw 的 body 是多行原文；空行是 SSE 帧分隔符，必须原样发出。
// 曾经丢弃空行导致命名事件流（event: X + data: Y + 空行）全挤进一帧，
// traework 的正文因此全部为空。
func TestSSERawKeepsBlankLine(t *testing.T) {
	body := strings.Join([]string{
		"event: output",
		`data: {"response":"第一段"}`,
		"",
		"event: output",
		`data: {"response":"第二段"}`,
		"",
		"event: done",
		`data: {"finish_reason":"stop"}`,
		"",
	}, "\n")
	s, err := New([]Route{{Method: "POST", Path: "/chat", Kind: KindSSERaw, Body: body}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	out := post(t, s.Base, "/chat")
	if n := strings.Count(out, "\n\n"); n < 3 {
		t.Fatalf("应保留 3 个空行分隔帧，实得 %d；输出:\n%s", n, out)
	}
	if !strings.Contains(out, "event: output") || !strings.Contains(out, "第二段") {
		t.Fatalf("命名事件帧丢失:\n%s", out)
	}
}

// NoUsage 必须能从预烘的 SSE 原文里剥掉 usage 帧，否则「上游不报 usage」的场景
// 无法构造（gemini 把 usageMetadata 写死在末帧，曾因此误判「臆造用量」）。
func TestSSERawNoUsageStripsUsageFrames(t *testing.T) {
	body := strings.Join([]string{
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}}`,
		"",
		"event: token_usage",
		`data: {"prompt_tokens":12,"completion_tokens":9}`,
		"",
		`data: {"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":9}}`,
		"",
		`data: {"usage":{"prompt_tokens":12}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	s, err := New([]Route{{Method: "POST", Path: "/chat", Kind: KindSSERaw, Body: body, NoUsage: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	out := post(t, s.Base, "/chat")
	for _, bad := range []string{"prompt_tokens", "usageMetadata", "token_usage"} {
		if strings.Contains(out, bad) {
			t.Fatalf("NoUsage 未剥掉 %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "hi") || !strings.Contains(out, "[DONE]") {
		t.Fatalf("正文或终止帧被误删:\n%s", out)
	}
}

// OpenAI 形态的 usage 帧同样受 NoUsage 控制。
func TestSSEOpenAINoUsage(t *testing.T) {
	for _, noUsage := range []bool{false, true} {
		s, err := New([]Route{{
			Method: "POST", Path: "/v1/chat/completions", Kind: KindSSEOpenAI,
			Text: "你好世界", Chunks: 2, NoUsage: noUsage,
			Usage: &Usage{Prompt: 13, Completion: 7, Cached: 4},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		out := post(t, s.Base, "/v1/chat/completions")
		s.Close()
		hasUsage := strings.Contains(out, `"usage"`)
		if noUsage && hasUsage {
			t.Fatalf("NoUsage=true 时不应有 usage 帧:\n%s", out)
		}
		if !noUsage && !hasUsage {
			t.Fatalf("NoUsage=false 时应有 usage 帧:\n%s", out)
		}
		if !strings.Contains(out, "你好") {
			t.Fatalf("正文缺失:\n%s", out)
		}
	}
}

// 未命中路由必须回 404 并记录，便于反查插件真实请求形状。
func TestUnmatchedRouteRecorded(t *testing.T) {
	s, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	resp, err := http.Get(s.Base + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("未命中应回 404，实得 %d", resp.StatusCode)
	}
	if s.Find("GET", "/nope") == nil {
		t.Fatal("未命中请求应被记录")
	}
}
