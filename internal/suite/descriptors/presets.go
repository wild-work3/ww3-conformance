// Package descriptors 提供「内置预设」这一层（C 层，PLAN §2）。
//
// 本包**刻意不含任何渠道名**：19 个渠道的事实（mock 路由、夹具模型、凭据夹具、
// 能力缺口）全部是 `presets/*.json` 里的数据（改造前硬编码渠道事实的一次性导出物）。
//
// 这样做的目的（W3-53 用户定案）：
//   - 工具核心零渠道知识 —— 它不知道也不该知道「glm 的单轮限制」「qoder 的信封形状」；
//   - 删掉整个 presets/ 目录，`--exe` 单插件模式照样完整可用；
//   - 第三方插件永远不需要改本仓代码，写自己的 conformance.json 即可。
package descriptors

import (
	"sort"

	"github.com/wild-work3/ww3-conformance/internal/suite"
)

// All 返回全部内置预设（按 ID 排序）。
//
// distDir 只用于拼出 exe 路径（预设里的 exe 文件名约定为 `<预设ID>-ext`，见 presetExe）；
// liveAuthDir 非空时给每个预设填上 `<dir>/<id>.json` 作为真实凭据候选。
func All(distDir, liveAuthDir string) []suite.Descriptor {
	names := suite.ListPresets()
	out := make([]suite.Descriptor, 0, len(names))
	for _, n := range names {
		d, err := suite.LoadPreset(n, presetExe(distDir, n), nil)
		if err != nil {
			// 预设坏掉不该让整轮静默少一个渠道：保留占位，Run 会因 manifest 不可用而明确 FAIL。
			d = suite.Descriptor{ID: n, Kind: "channel", Exe: presetExe(distDir, n),
				Notes: []string{"预设载入失败: " + err.Error()}}
		}
		if liveAuthDir != "" {
			d.LiveAuthFile = defaultLiveAuth(liveAuthDir, d.ID)
		}
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// defaultLiveAuth 在凭据目录里找与预设 ID 同名的凭据文件（如 <dir>/glm.json）。
func defaultLiveAuth(dir, id string) string { return joinPath(dir, id+".json") }
