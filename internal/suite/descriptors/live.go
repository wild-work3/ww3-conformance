package descriptors

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ResolveAuth 把「凭据文件名 / 相对路径 / 绝对路径」解析成真实文件路径。
//
// 支持的写法（按顺序尝试）：
//   - 绝对路径、或含路径分隔符、或以 .json 结尾 → 原样（相对路径按当前目录）
//   - <data>/auths/<name>.json   ← 标准布局（W3-21：<data>/auths/）
//   - <data>/<name>.json          ← 紧凑布局（dev/dist 时代把 auths 当根）
//   - <data>/auths/<name>         ← 已带后缀
func ResolveAuth(dataDir, name string) string {
	if name == "" {
		return ""
	}
	if strings.ContainsAny(name, `/\`) || strings.HasSuffix(strings.ToLower(name), ".json") ||
		filepath.IsAbs(name) {
		return name
	}
	cands := []string{
		filepath.Join(dataDir, "auths", name+".json"),
		filepath.Join(dataDir, name+".json"),
		filepath.Join(dataDir, "auths", name),
	}
	for _, p := range cands {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return cands[0] // 不存在也返回首选路径，让上层报「凭据不存在」并写清位置
}

// DiscoverAuths 在 dataDir 下按前缀发现凭据文件，按修改时间新→旧排序。
//
// 只看两个位置：<data>/auths/*.json（标准）与 <data>/*.json（dev/dist 时代布局）。
// 前缀为空时回退到 id。发现多个时**只取最新的一个**：真实上游每多打一次就多
// 一分风控风险，用户明确要求单轮、少请求。
func DiscoverAuths(dataDir string, prefixes []string, id string) []string {
	if dataDir == "" {
		return nil
	}
	if len(prefixes) == 0 {
		prefixes = []string{id}
	}
	var dirs []string
	for _, d := range []string{filepath.Join(dataDir, "auths"), dataDir} {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			dirs = append(dirs, d)
		}
	}
	type hit struct {
		path string
		mod  int64
	}
	var hits []hit
	for _, dir := range dirs {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
				continue
			}
			base := strings.TrimSuffix(e.Name(), ".json")
			for _, p := range prefixes {
				if strings.HasPrefix(base, p+"-") || base == p {
					info, ierr := e.Info()
					var mod int64
					if ierr == nil {
						mod = info.ModTime().Unix()
					}
					hits = append(hits, hit{filepath.Join(dir, e.Name()), mod})
					break
				}
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].mod > hits[j].mod })
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.path)
	}
	return out
}
