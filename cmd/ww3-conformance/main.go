// Command ww3-conformance 是 ww3 渠道 / 拦截器插件的契约符合性自检工具。
//
// **它不是准入门禁**（用户 m02550 / m02729）：只交付工具，每个插件仓自己决定要不要在
// workflow 里跑；注册表只校格式，不校这里的结果。有摘要就多显示一点，没有就照常展示。
//
// 工具核心**零渠道知识**：渠道事实（mock 路由、夹具模型、能力缺口）全部来自
// `conformance.json`（作者自带）或 `presets/*.json`（工作区数据），删掉 presets 也能跑 --exe。
//
// 三种用法（三层可测深度，PLAN §2）：
//
//	A 零配置：ww3-conformance --exe ../dist/workbuddy-ext.exe
//	  只验契约骨架（manifest / call / stdout 纯 JSONL / lifecycle / 能力一致性），
//	  **零网络**：无 mock ⇒ 控制面与会话面自动跳过（绝不静默打真上游）。
//
//	B 自带描述文件：ww3-conformance --exe <程序> --descriptor conformance.json
//	  加上控制面 + 会话面 + 多轮 + 用量口径，全部打本地 mock 上游。
//
//	C 内置预设：ww3-conformance --dist ../dist            （全部预设）
//	           ww3-conformance --dist ../dist --only glm （单个预设）
//	  与改造前行为一致（预设就是原来的 19 份渠道事实，已降级为 JSON 数据）。
//
// 非单 exe 插件（Python / Node）：
//
//	ww3-conformance --exe /path/to/python -- /path/to/myext/main.py
//	实际启动：/path/to/python /path/to/myext/main.py call --data <夹具目录>
//	`--` 之前是 conformance 的参数，之后全部透传给被测进程（用户 m02793 / m02861）。
//
// 退出码：0 全部 PASS/WARN；1 存在 FAIL；2 用法错误。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/wild-work3/ww3-conformance/internal/check"
	"github.com/wild-work3/ww3-conformance/internal/report"
	"github.com/wild-work3/ww3-conformance/internal/suite"
	"github.com/wild-work3/ww3-conformance/internal/suite/descriptors"
)

func main() {
	// `--` 之前是 conformance 的参数，之后全部透传给被测进程（W3-51 §3.1.1）。
	// Go 的 flag 包遇到第一个非 flag 参数就停止解析，所以必须自己先切一刀。
	own, pluginArgs := splitArgs(os.Args[1:])

	var (
		dist      = flag.String("dist", "../dist", "构建产物目录（含各插件 exe；与 --exe 互斥）")
		exe       = flag.String("exe", "", "单插件模式：被测程序路径（可以是解释器，如 python.exe）")
		descriptor = flag.String("descriptor", "", "描述文件路径（缺省：--exe 时在 exe 同目录找 conformance.json）")
		idFlag    = flag.String("id", "", "显式插件 ID（缺省从 manifest.id 取）")
		only      = flag.String("only", "", "只跑指定插件（逗号分隔；预设模式与 --exe 都可用）")
		skip      = flag.String("skip", "", "跳过指定插件（逗号分隔）")
		mode      = flag.String("mode", "mock", "mock | live（offline 是 mock 的别名）")
		outDir    = flag.String("out", "reports", "报告输出目录")
		workDir   = flag.String("work", "", "夹具临时根目录（空则用系统临时目录）")
		keep      = flag.Bool("keep", false, "保留夹具临时目录（排查用）")
		verbose   = flag.Bool("v", false, "打印每个断言")
		listOnly  = flag.Bool("list", false, "只列出可跑的插件并退出")
		planOnly  = flag.Bool("plan", false, "只打印 live 计划（渠道→凭据→模型）并退出，不发起任何请求")
		liveAuth  = flag.String("live-auth", "", "旧写法：凭据目录下的 <插件ID>.json（等价于 --data + 自动发现）")
		liveData  = flag.String("data", "", "真实凭据根目录（含 auths/*.json；也兼容 dev/dist 的 <dir>/<名字>.json）")
		liveName  = flag.String("auth", "", "真实凭据文件名（去 .json；如 glm-6982e0ad0efeac7998789a48）")
		liveModel = flag.String("model", "", "真实对话用的模型 ID（覆盖描述符默认；只能配单插件）")
		liveTurns = flag.Int("live-turns", 1, "真实上游对话轮数：1=只跑一轮（默认，避免风控），2=再验一轮上下文")
		summaryTo = flag.String("json-summary", "", "输出可直接贴进插件注册表的摘要 JSON（缺省 <out>/conformance-summary.json）")
		noSummary = flag.Bool("no-summary", false, "不写摘要文件")
		snippet   = flag.Bool("print-snippet", false, "把摘要渲染成可粘贴的 conformance/capabilities 片段打到 stdout")
		strict    = flag.Bool("strict", false, "把 WARN 也算失败（默认不算）")
		timeout   = flag.Duration("timeout", 20*time.Minute, "整体超时")
	)
	flag.CommandLine.SetOutput(os.Stderr)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法：ww3-conformance [开关…] [-- <透传给被测插件的参数…>]\n\n")
		fmt.Fprintf(os.Stderr, "三种用法：\n")
		fmt.Fprintf(os.Stderr, "  --exe <程序> [-- <插件参数>]        单插件零配置（A 层，零网络）\n")
		fmt.Fprintf(os.Stderr, "  --exe <程序> --descriptor <文件>    单插件 + 描述文件（B 层，全打本地 mock）\n")
		fmt.Fprintf(os.Stderr, "  --dist <目录>                       跑内置预设（C 层，原 19 渠道行为）\n\n")
		flag.PrintDefaults()
	}
	// 只解析 `--` 之前的部分：`--` 之后的全部归被测插件（W3-51 §3.1.1）。
	// 直接 flag.Parse() 会把 `--` 之后的当成多余位置参数。
	flag.CommandLine.Parse(own)
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "多余的参数：%v（插件参数要写在 `--` 之后）\n", flag.Args())
		os.Exit(2)
	}

	m := check.ModeOffline
	if strings.EqualFold(*mode, "live") {
		m = check.ModeLive
	} else if s := strings.ToLower(*mode); s != "mock" && s != "offline" {
		fmt.Fprintf(os.Stderr, "未知 --mode %q（可选 mock|live）\n", *mode)
		os.Exit(2)
	}

	// --exe 与 --dist 互斥：单插件模式不接受 --dist 的目录语义。
	if *exe != "" && *dist != "../dist" && *dist != "" {
		fmt.Fprintln(os.Stderr, "--exe 与 --dist 不能同时使用")
		os.Exit(2)
	}
	// 预设模式（--dist）不接受 `--` 之后的参数：多插件共用一份启动参数没有语义。
	if *exe == "" && len(pluginArgs) > 0 {
		fmt.Fprintln(os.Stderr, "`--` 之后的参数只用于 --exe 单插件模式（--dist 多插件共用一份参数没有语义）")
		os.Exit(2)
	}

	picked, err := pickDescriptors(*exe, *descriptor, *idFlag, *dist, *liveAuth, pluginArgs, *only, *skip)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(2)
	}

	if *listOnly {
		for _, d := range picked {
			src := d.Exe
			if len(d.Args) > 0 {
				src += " " + strings.Join(d.Args, " ")
			}
			fmt.Printf("%-14s %-8s %s\n", d.ID, orDefault(d.Kind, "channel"), src)
		}
		return
	}

	if *liveModel != "" && !strings.EqualFold(*liveModel, "auto") && len(picked) > 1 {
		fmt.Fprintln(os.Stderr, "--model 只能配合单插件使用（多插件请用 --model auto 让各插件自选 flash 模型）")
		os.Exit(2)
	}
	if *liveName != "" && len(picked) > 1 {
		fmt.Fprintln(os.Stderr, "--auth 只能配合单插件使用（先用 --only 指定一个；多插件请用 --data 自动发现）")
		os.Exit(2)
	}
	if m == check.ModeLive && *liveTurns < 1 {
		fmt.Fprintln(os.Stderr, "--live-turns 必须 >= 1")
		os.Exit(2)
	}

	// --plan：只打印将要用哪份凭据/模型（避免先打一发才发现配错）
	if *planOnly {
		fmt.Printf("%-14s %-6s %-46s %s\n", "插件", "模式", "凭据", "模型")
		for _, d := range picked {
			if *liveName != "" {
				d.LiveAuthFile = descriptors.ResolveAuth(*liveData, *liveName)
			} else if *liveData != "" {
				if hits := descriptors.DiscoverAuths(*liveData, d.AuthPrefixes, d.ID); len(hits) > 0 {
					d.LiveAuthFile = hits[0]
				}
			}
			if *liveModel != "" {
				d.LiveModel = *liveModel
			}
			cred := filepath.Base(d.LiveAuthFile)
			if d.LiveAuthFile == "" {
				if d.LiveAnon {
					cred = "（匿名，无需凭据）"
				} else {
					cred = "（无凭据 → live 会 FAIL）"
				}
			}
			fmt.Printf("%-14s %-6s %-46s %s\n", d.ID, "live", cred, d.LiveModel)
		}
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, cancel2 := context.WithTimeout(ctx, *timeout)
	defer cancel2()

	b := report.New(m)
	if *exe != "" {
		b.Env = append(b.Env, "被测程序： "+*exe)
		if len(pluginArgs) > 0 {
			b.Env = append(b.Env, "透传参数： "+strings.Join(pluginArgs, " "))
		}
		if *descriptor != "" {
			b.Env = append(b.Env, "描述文件： "+*descriptor)
		} else if len(picked) > 0 && len(picked[0].Routes) == 0 {
			b.Env = append(b.Env, "未提供描述文件：只跑契约骨架（A 层，零网络）")
		}
	}
	if m == check.ModeLive {
		if *liveData != "" {
			b.Env = append(b.Env, "真实凭据根： "+*liveData)
		}
		if *liveName != "" {
			b.Env = append(b.Env, "指定凭据： "+*liveName)
		}
		if *liveModel != "" {
			b.Env = append(b.Env, "指定模型： "+*liveModel)
		}
		b.Env = append(b.Env, fmt.Sprintf("真实请求轮数： %d（每插件最多 %d 次上游请求）", *liveTurns, *liveTurns))
		if px := strings.TrimSpace(os.Getenv("WW3_LIVE_PROXY")); px != "" {
			b.Env = append(b.Env, "代理： "+px)
		}
	}
	for i, d := range picked {
		if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "\n整体超时/中断，跳过剩余 %d 个插件\n", len(picked)-i)
			break
		}
		// live 模式：解析出**一条**凭据（用户要求只跑一轮，不多账号重试，避免风控）
		if m == check.ModeLive {
			applyLiveCredential(&d, *liveData, *liveName)
			if *liveModel != "" {
				d.LiveModel = *liveModel
			}
		}
		fmt.Fprintf(os.Stderr, "[%d/%d] %s …\n", i+1, len(picked), d.ID)
		rep := suite.Run(ctx, d, suite.Options{Mode: m, Keep: *keep, WorkDir: *workDir,
			Verbose: *verbose, LiveTurns: *liveTurns})
		if m == check.ModeLive && d.LiveAuthFile != "" {
			rep.Notes = append(rep.Notes, "真实凭据："+filepath.Base(d.LiveAuthFile))
		}
		b.Add(rep)
		fmt.Fprintf(os.Stderr, "        %s  %s\n", rep.Verdict(), rep.Summary())
		if *verbose {
			for _, a := range rep.Checks {
				fmt.Fprintf(os.Stderr, "          %-4s %-28s %s\n", a.Status, a.ID, a.Detail)
			}
		}
	}
	b.Finish()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "建报告目录失败: %v\n", err)
		os.Exit(2)
	}
	stamp := time.Now().Format("20060102-150405")
	md := filepath.Join(*outDir, "conformance-"+string(m)+"-"+stamp+".md")
	js := filepath.Join(*outDir, "conformance-"+string(m)+"-"+stamp+".json")
	// 同时写一份稳定文件名，便于文档引用最新结果。
	mdLatest := filepath.Join(*outDir, "conformance-latest.md")
	if err := b.WriteMarkdown(md); err != nil {
		fmt.Fprintf(os.Stderr, "写 markdown 报告失败: %v\n", err)
	}
	if err := b.WriteMarkdown(mdLatest); err != nil {
		fmt.Fprintf(os.Stderr, "写 markdown 报告失败: %v\n", err)
	}
	if err := b.WriteJSON(js); err != nil {
		fmt.Fprintf(os.Stderr, "写 JSON 报告失败: %v\n", err)
	}

	// 注册表可用摘要（W3-51 §3.3）：工具的输出，不是门禁的判决书。
	if !*noSummary {
		p := *summaryTo
		if p == "" {
			p = filepath.Join(*outDir, "conformance-summary.json")
		}
		if err := b.WriteSummary(p); err != nil {
			fmt.Fprintf(os.Stderr, "写摘要失败: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "摘要: %s\n", p)
		}
		if *snippet {
			if s := b.Snippet(); s != "" {
				fmt.Println(s)
			}
		}
	}

	tot := b.Totals()
	fmt.Fprintf(os.Stderr, "\n汇总: PASS=%d FAIL=%d WARN=%d SKIP=%d → %s\n",
		tot[check.Pass], tot[check.Fail], tot[check.Warn], tot[check.Skip], b.Verdict())
	fmt.Fprintf(os.Stderr, "报告: %s\n      %s\n", md, js)

	if b.Verdict() == check.Fail || (*strict && tot[check.Warn] > 0) {
		os.Exit(1)
	}
}

// splitArgs 在 os.Args 里找第一个 `--`，切成「conformance 自己的参数」与「透传给插件的参数」。
func splitArgs(argv []string) (own, plugin []string) {
	for i, a := range argv {
		if a == "--" {
			return argv[:i], argv[i+1:]
		}
	}
	return argv, nil
}

// pickDescriptors 按模式选出要跑的插件清单。
//
// 三种来源互斥：--exe（单插件）/ --dist（内置预设）/ 都为空（缺省 --dist ../dist）。
func pickDescriptors(exe, descriptor, idFlag, dist, liveAuth string, pluginArgs []string,
	only, skip string) ([]suite.Descriptor, error) {
	onlySet, skipSet := splitSet(only), splitSet(skip)

	// ---- 单插件模式（A / B 层）----
	if exe != "" {
		var d suite.Descriptor
		var err error
		if descriptor != "" {
			d, err = suite.LoadFile(descriptor, exe, pluginArgs)
			if err != nil {
				return nil, fmt.Errorf("载入描述文件失败: %w", err)
			}
		} else if p := siblingDescriptor(exe); p != "" {
			// 作者把 conformance.json 放在插件旁边时自动发现（零参数也能到 B 层）
			d, err = suite.LoadFile(p, exe, pluginArgs)
			if err != nil {
				return nil, fmt.Errorf("载入自动发现的描述文件 %s 失败: %w", p, err)
			}
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			d, err = suite.Auto(ctx, exe, pluginArgs)
			if err != nil {
				return nil, err
			}
		}
		if idFlag != "" {
			d.ID = idFlag
		}
		if len(onlySet) > 0 && !onlySet[d.ID] {
			return nil, fmt.Errorf("插件 ID %q 不在 --only 里", d.ID)
		}
		if skipSet[d.ID] {
			return nil, fmt.Errorf("插件 %q 在 --skip 里", d.ID)
		}
		return []suite.Descriptor{d}, nil
	}

	// ---- 预设模式（C 层，行为与改造前一致）----
	all := descriptors.All(dist, liveAuth)
	if len(all) == 0 {
		return nil, fmt.Errorf("没有内置预设（%s 下的 presets/*.json 为空？）", "presets")
	}
	var picked []suite.Descriptor
	for _, d := range all {
		if len(onlySet) > 0 && !onlySet[d.ID] {
			continue
		}
		if skipSet[d.ID] {
			continue
		}
		picked = append(picked, d)
	}
	if len(picked) == 0 {
		return nil, fmt.Errorf("没有可跑的插件（检查 --only/--skip 与 --dist）")
	}
	return picked, nil
}

// siblingDescriptor 在 exe 同目录找 conformance.json（B 层的零参数入口）。
func siblingDescriptor(exe string) string {
	p := filepath.Join(filepath.Dir(exe), "conformance.json")
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p
	}
	return ""
}

// applyLiveCredential 为插件解析唯一一条真实凭据（--auth 显式指定优先，否则按前缀
// 自动发现并取最新一份）。刻意不重试多份凭据：每多打一次真实上游就多一分风控风险。
func applyLiveCredential(d *suite.Descriptor, dataDir, name string) {
	if name != "" {
		d.LiveAuthFile = descriptors.ResolveAuth(dataDir, name)
		return
	}
	if dataDir != "" {
		if hits := descriptors.DiscoverAuths(dataDir, d.AuthPrefixes, d.ID); len(hits) > 0 {
			d.LiveAuthFile = hits[0]
		}
	}
}

func splitSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	return out
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// 保证 sort 被使用（descriptors.All 已排序，这里仅防御性再排一次）。
var _ = sort.Strings
