// exe.go 把「预设 ID → 可执行文件路径」这条约定集中在一处。
//
// 约定（与 build.sh / layout_dist_plugins.sh 的产物名一致）：
//   - 渠道插件：`<id>-ext`（workbuddy → workbuddy-ext.exe）
//   - hook 插件：`<id>-hook`（loopguard → loopguard-hook.exe）
//   - 例外：`-cli` 型插件（chatgpt-cli / chatglm-cli）交付物就是自己的名字
//
// 这是**文件名约定**，不是渠道知识——所以放在这里而不是 presets/ 里。
// 找不到时依次回退：约定名 → `<id>` 裸名 → 目录里唯一匹配项（resolveExe 三级解析同源）。
package descriptors

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// withExt 给文件名补上平台后缀（Windows 加 .exe）。
func withExt(name string) string {
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(name), ".exe") {
		return name + ".exe"
	}
	return name
}

// joinPath 是 filepath.Join 的短别名（本包多处拼路径）。
func joinPath(parts ...string) string { return filepath.Join(parts...) }

// presetExe 按约定拼出预设对应的可执行文件路径。
//
// 三级回退（任一命中即返回）：
//  1. 约定名：`-cli` 后缀的预设用原名；其余用 `<id>-ext`
//  2. 裸名：`<id>`
//  3. 目录扫描：`<id>-ext.*` / `<id>.*` 的第一个匹配
//
// 都找不到时返回第 1 步的路径（让上层报「文件不存在」并写清期望位置）。
func presetExe(distDir, id string) string {
	cands := presetExeCands(distDir, id)
	for _, p := range cands {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	// 目录扫描兜底：文件名以 `<id>-ext` 或 `<id>` 开头的可执行文件
	if ents, err := os.ReadDir(distDir); err == nil {
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			n := e.Name()
			if !strings.HasSuffix(strings.ToLower(n), exeSuffix()) {
				continue
			}
			stem := strings.TrimSuffix(strings.TrimSuffix(n, ".exe"), filepath.Ext(n))
			if stem == id || stem == id+"-ext" || stem == id+"-hook" || stem == id+"-cli" {
				return filepath.Join(distDir, n)
			}
		}
	}
	return cands[0]
}

// presetExeCands 返回按优先级排列的候选路径。
func presetExeCands(distDir, id string) []string {
	var names []string
	if strings.HasSuffix(id, "-cli") {
		names = append(names, id) // chatgpt-cli / chatglm-cli 交付物就是自己的名字
	} else {
		names = append(names, id+"-ext", id+"-hook")
	}
	names = append(names, id) // 裸名兜底
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, filepath.Join(distDir, withExt(n)))
	}
	return out
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
