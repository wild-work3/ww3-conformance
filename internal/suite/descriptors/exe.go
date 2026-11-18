// exe.go 把「预设 ID → 可执行文件路径」这条**本仓预设约定**集中在一处。
//
// **插件可执行文件名不是契约**（用户 m00634/m00642：曾多次强调“不能假定插件后缀一定是
// -ext / -hook”）。身份以 manifest.id 为准，产物名完全自由：`chatgpt-cli`、`chatglm-cli`、
// `python.exe main.py` 均合法；core 侧靠 resolveExe 三级解析发现，`store.Install` 不校文件名。
//
// 约定仅用于**本仓 19 个内置预设**（与 build.sh 产物名一致，写在这里是为了能一行跑全渠道）：
//   - 渠道插件：`<id>-ext`（workbuddy → workbuddy-ext.exe）
//   - hook 插件：`<id>-hook`（loopguard → loopguard-hook.exe）
//   - 例：`-cli` 型插件（chatgpt-cli / chatglm-cli）交付物就是自己的名字
//
// 第三方插件不走这个约定：自己的 conformance.json 写 `"exe"` 字段，或直接 `--exe <路径>`。
// 找不到时依次回退：约定名 → `<id>` 裸名 → 目录扫描（与 core 的 resolveExe 同思想）。
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
// **这是本仓维护的预设约定，不是插件契约**（用户 m00634/m00642）：插件产物叫什么名字完全自由，
// 身份以 manifest.id 为准。第三方作者若产物名不在下列约定内，可在自己的 conformance.json
// 里写 `"exe": "<文件名>"`（相对 --dist），或直接用 `--exe <路径>` 绕过本函数。
//
// 三级回退（任一命中即返回）：
//  1. 约定名：`-cli` 后缀的预设用原名；其余用 `<id>-ext` / `<id>-hook`
//  2. 裸名：`<id>`
//  3. 目录扫描：词干等于 id / `<id>-ext` / `<id>-hook` / `<id>-cli` 的第一个可执行文件
//
// 都找不到时返回第 1 步的路径（让上层报「文件不存在」并写清期望位置）。
//
// W3-58：安装根布局是 `dist/plugins/<id>@<kind>/`（单版本、无 manifest.json），
// 因此先扫插件目录树，再回退到「平铺在 --dist 根」的老布局（便于单独跑产物目录）。
func presetExe(distDir, id string) string {
	if p := exeInPluginTree(distDir, id); p != "" {
		return p
	}
	// -cli 型预设的 ID（chatglm-cli）与插件 ID（glm）不同，插件树里按目录名找不到，
	// 直接扫 `dist/plugins/*@channel/` 里词干匹配的文件。
	if p := exeInPluginTreeByStem(distDir, id); p != "" {
		return p
	}
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

// exeInPluginTree 在 `dist/plugins/<id>@<kind>/` 里找入口（W3-58 安装布局）。
func exeInPluginTree(distDir, id string) string {
	ents, err := os.ReadDir(joinPath(distDir, "plugins"))
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if base, _, ok := strings.Cut(name, "@"); !ok || base != id {
			continue
		}
		dir := joinPath(distDir, "plugins", name)
		// 有 entry.json（多文件/解释器插件）时无法只看文件名，交由 --exe 或 conformance.json 指定
		if _, err := os.Stat(joinPath(dir, "entry.json")); err == nil {
			return ""
		}
		if p := firstExeIn(dir); p != "" {
			return p
		}
	}
	return ""
}

// exeInPluginTreeByStem 按「产物词干」在插件树里找（预设 ID 可能是仓名而非插件 ID，如 chatglm-cli）。
func exeInPluginTreeByStem(distDir, id string) string {
	ents, err := os.ReadDir(joinPath(distDir, "plugins"))
	if err != nil {
		return ""
	}
	stems := []string{id, id + "-ext", id + "-hook"}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := joinPath(distDir, "plugins", e.Name())
		if _, err := os.Stat(joinPath(dir, "entry.json")); err == nil {
			continue
		}
		p := firstExeIn(dir)
		if p == "" {
			continue
		}
		stem := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		for _, want := range stems {
			if stem == want {
				return p
			}
		}
	}
	return ""
}

// firstExeIn 返回目录根下唯一/首个可执行文件（跳过 entry.json、*.bak.zip 等非入口文件）。
func firstExeIn(dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var cands []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(strings.ToLower(n), ".json") || strings.HasSuffix(strings.ToLower(n), ".zip") {
			continue
		}
		if runtime.GOOS == "windows" {
			switch strings.ToLower(filepath.Ext(n)) {
			case ".exe", ".cmd", ".bat":
				cands = append(cands, n)
			}
			continue
		}
		if filepath.Ext(n) == "" || strings.HasSuffix(n, ".sh") {
			cands = append(cands, n)
		}
	}
	if len(cands) == 0 {
		return ""
	}
	return joinPath(dir, cands[0])
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
