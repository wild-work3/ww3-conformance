// serve.go 是 `serve` 命令（UI 平台化，W3-61）的进程级契约辅助。
//
// serve 与 call/stdio 完全不同：它不是 JSONL-RPC 会话，而是「stdout 写一条 serve-ready
// 就绪帧 → 长驻回环 HTTP」；就绪帧没有 id、且之后 stdout 必须静默，因此**不能**复用
// Client 的 readLoop（那会按 RPC 帧语义解析、等待 id 配对）。这里做独立的轻量封装。
//
// 契约出处：protocol/docs/PLUGIN_CLI.md §2（serve 命令）与
// protocol/docs/PLUGIN_PROTOCOL.md §3.1.1（UI slot）。
package wire

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ServeReady 是 serve 命令写在 stdout 第一行的就绪帧。
//
// 刻意不复用 Frame：Frame 没有 Listen 字段，直接 Unmarshal 会**静默丢掉 listen**
// （踩过的坑，所以单独定义）。
type ServeReady struct {
	V      int    `json:"v"`
	Op     string `json:"op"`
	Listen string `json:"listen"`
}

// ServeProc 是一个跑着 `serve` 的插件子进程。
//
// 生命周期：SpawnServe 读到就绪帧后返回；调用方用 HTTP 打它，结束后必须 Kill/Wait，
// 否则准入跑完会留下孤儿 serve 进程（W3-18「进程零残留」纪律）。
type ServeProc struct {
	Ready ServeReady
	// ReadyLine 是就绪帧原文（诊断用）。
	ReadyLine string

	cmd     *exec.Cmd
	stderr  *syncBuffer
	mu      sync.Mutex
	extra   []string // 就绪帧之后的 stdout 行（契约要求恒为空）
	eof     bool     // stdout 读完了（进程退出或关闭了 stdout）
	dead    chan struct{}
	waitErr error
}

// SpawnServe 启动 `serve` 并等待就绪帧。
//
// args 是**除 exe 外**的完整命令行（含子命令与全部开关）。
// readyTimeout 内没读到合法就绪帧 → 进程被杀并返回错误。
func SpawnServe(ctx context.Context, name, exe string, args []string, readyTimeout time.Duration) (*ServeProc, error) {
	if readyTimeout <= 0 {
		readyTimeout = 10 * time.Second
	}
	cmd := exec.Command(exe, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("打开 stdout 失败: %w", err)
	}
	sb := &syncBuffer{}
	cmd.Stderr = sb
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 %s 失败: %w", name, err)
	}
	p := &ServeProc{cmd: cmd, stderr: sb, dead: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.dead)
	}()
	// ctx 取消时杀进程，保证不挂孤儿
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
		case <-p.dead:
		}
	}()

	// 读第一行（就绪帧），带超时
	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 0, 1<<20), MaxFrame)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			lineCh <- line
			// 继续扫后续行：契约要求就绪帧之后 stdout 静默，多出来的行记进 extra
			for sc.Scan() {
				if l := strings.TrimSpace(sc.Text()); l != "" {
					p.mu.Lock()
					p.extra = append(p.extra, l)
					p.mu.Unlock()
				}
			}
			p.mu.Lock()
			p.eof = true
			p.mu.Unlock()
			return
		}
		p.mu.Lock()
		p.eof = true
		p.mu.Unlock()
		errCh <- sc.Err()
	}()

	select {
	case line := <-lineCh:
		p.ReadyLine = line
		var r ServeReady
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			p.Kill()
			return nil, fmt.Errorf("serve-ready 帧不是合法 JSON: %s", truncate(line, 300))
		}
		if r.Op != "serve-ready" {
			p.Kill()
			return nil, fmt.Errorf("stdout 首帧 op=%q（应为 serve-ready）: %s", r.Op, truncate(line, 300))
		}
		if r.Listen == "" {
			p.Kill()
			return nil, fmt.Errorf("serve-ready 帧缺 listen 字段: %s", truncate(line, 300))
		}
		p.Ready = r
		return p, nil
	case <-errCh:
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("serve 未输出就绪帧就退出了（stderr: %s）", truncate(sb.String(), 400))
	case <-time.After(readyTimeout):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("等待 serve-ready 超时 %v（stderr: %s）", readyTimeout, truncate(sb.String(), 400))
	}
}

// ExtraLines 返回就绪帧之后的 stdout 行（契约要求恒为空）。
func (p *ServeProc) ExtraLines() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.extra...)
}

// StdoutEOF 报告 stdout 是否已读到 EOF（serve 进程退出或关闭 stdout 都算）。
func (p *ServeProc) StdoutEOF() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.eof
}

// Alive 报告进程是否仍在运行。
func (p *ServeProc) Alive() bool {
	select {
	case <-p.dead:
		return false
	default:
		return true
	}
}

// Stderr 返回子进程累计的 stderr。
func (p *ServeProc) Stderr() string { return p.stderr.String() }

// Kill 杀掉子进程（不等待退出；等退出用 Wait）。
func (p *ServeProc) Kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// Wait 等子进程退出，返回退出码。
func (p *ServeProc) Wait(timeout time.Duration) (int, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	select {
	case <-p.dead:
		return p.cmd.ProcessState.ExitCode(), p.waitErr
	case <-time.After(timeout):
		p.Kill()
		return -1, fmt.Errorf("serve 进程 %v 内未退出（已强杀）", timeout)
	}
}
