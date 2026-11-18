// ui_test.go 是 UI 层纯逻辑的单元测试（不拉进程；进程级断言由夹具 temp/uiserve 端到端验证）。
package suite

import (
	"testing"
)

func TestScanRootAbsolute(t *testing.T) {
	cases := []struct {
		html string
		want []string
	}{
		// 命中：根绝对路径资源
		{`<script src="/assets/app.js"></script>`, []string{"/assets/app.js"}},
		{`<link rel="stylesheet" href='/css/main.css'>`, []string{"/css/main.css"}},
		{`<div style="background:url(/img/bg.png)">`, []string{"/img/bg.png"}},
		{`<div style="background:url('/img/bg.png')">`, []string{"/img/bg.png"}},
		// 不命中：相对路径 / 协议相对 / 自引用 / hash / query
		{`<script src="app.js"></script>`, nil},
		{`<link href="./style.css">`, nil},
		{`<script src="//cdn.example.com/x.js"></script>`, nil},
		{`<a href="/">首页</a>`, nil},
		{`<a href="#top">顶部</a>`, nil},
		{`<a href="?debug=1">调试</a>`, nil},
		{`<a href="https://example.com/x">外链</a>`, nil},
		// 混合：只报根绝对路径
		{`<img src="a.png"><img src="/b.png"><img src="//c.png">`, []string{"/b.png"}},
	}
	for _, c := range cases {
		got := scanRootAbsolute(c.html)
		if len(got) != len(c.want) {
			t.Errorf("scanRootAbsolute(%q) = %v, want %v", c.html, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("scanRootAbsolute(%q) = %v, want %v", c.html, got, c.want)
			}
		}
	}
}

func TestValidEntry(t *testing.T) {
	// 与主仓 meta.ValidEntry 同口径
	bad := []string{"", "/abs.html", "a//b", "../x", "./x", "a?b", "a#b", "a\\b", "http://x/y", "a/./b", "a/../b"}
	for _, e := range bad {
		if validEntry(e) {
			t.Errorf("validEntry(%q) = true, want false", e)
		}
	}
	good := []string{"index.html", "ui/index.html", "a-b/c_d.html"}
	for _, e := range good {
		if !validEntry(e) {
			t.Errorf("validEntry(%q) = false, want true", e)
		}
	}
}

func TestParseUI(t *testing.T) {
	m := map[string]any{
		"ui": map[string]any{
			"main": []any{map[string]any{"id": "home", "title": "主页", "entry": "index.html"}},
			"settings": []any{
				map[string]any{"id": "cfg", "title": "设置", "entry": "settings.html"},
			},
		},
	}
	slots, e := parseUI(m)
	if e != "" || len(slots) != 2 {
		t.Fatalf("parseUI = %v, err=%q", slots, e)
	}
	if slots[0].Pos != "main" || slots[1].Pos != "settings" {
		t.Errorf("slot 顺序错: %+v", slots)
	}
	// ui 缺失 / 非对象 → 空 + 无错误
	if slots, e := parseUI(map[string]any{}); len(slots) != 0 || e != "" {
		t.Errorf("无 ui 时 parseUI = %v, err=%q", slots, e)
	}
}

func TestSplitListen(t *testing.T) {
	h, p, err := splitListen("127.0.0.1:57530")
	if err != nil || h != "127.0.0.1" || p != "57530" {
		t.Errorf("splitListen = %q/%q/%v", h, p, err)
	}
	if _, _, err := splitListen("no-port"); err == nil {
		t.Errorf("缺端口应报错")
	}
}
