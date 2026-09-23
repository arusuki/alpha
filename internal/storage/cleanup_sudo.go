package storage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"project-alpha/internal/httpapi"
)

const sudoCleanupPrompt = "[project-alpha-cleanup-password]"

type cleanupHelperRequest struct {
	Paths          []string `json:"paths"`
	ProtectedTrees []string `json:"protected_trees"`
	ProtectedRoots []string `json:"protected_roots"`
	WritableLayers []string `json:"writable_layers"`
}
type cleanupHelperEvent struct {
	Type   string `json:"type"`
	Path   string `json:"path,omitempty"`
	Status string `json:"status,omitempty"`
	Code   string `json:"code,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Never retain sudo output: PAM/plug-ins may echo input in their diagnostics.
// Only the exact prompt is recognized; send one password and wipe it immediately.
// Waiting for a prompt also keeps credentials out of the helper's stdin when
// sudo requires no authentication (root or a NOPASSWD rule).
type sudoCleanupInput struct {
	mu          sync.Mutex
	input       io.WriteCloser
	password    []byte
	matched     int
	sent, ready bool
}

func (p *sudoCleanupInput) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range data {
		if b == sudoCleanupPrompt[p.matched] {
			p.matched++
		} else {
			p.matched = 0
			if b == sudoCleanupPrompt[0] {
				p.matched = 1
			}
		}
		if p.matched != len(sudoCleanupPrompt) {
			continue
		}
		p.matched = 0
		if p.sent || p.ready {
			_ = p.input.Close()
			continue
		}
		p.sent = true
		_, err := p.input.Write(p.password)
		clear(p.password)
		p.password = nil
		if err == nil {
			_, err = p.input.Write([]byte{'\n'})
		}
		if err != nil {
			_ = p.input.Close()
		}
	}
	return len(data), nil
}
func (p *sudoCleanupInput) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.password)
	p.password = nil
	p.ready = true
}

// One sudo process owns one confirmed batch. -k with a command ignores and does
// not update timestamp credentials. Neither passwords nor paths enter argv/env.
func runSudoCleanup(ctx context.Context, password []byte, request cleanupHelperRequest, result func(string, string, string) error, command func(context.Context, string, ...string) *exec.Cmd) error {
	defer clear(password)
	if err := ctx.Err(); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return httpapi.NewError(503, "无法定位删除辅助程序")
	}
	if command == nil {
		command = exec.CommandContext
	}
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := command(childCtx, "/usr/bin/sudo", "-S", "-k", "-p", sudoCleanupPrompt, "--", executable, "cleanup-helper")
	// No inherited terminal, askpass helper or diagnostic file. sudo only receives
	// the password through its private stdin pipe after issuing its prompt.
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	input, err := cmd.StdinPipe()
	if err != nil {
		return httpapi.NewError(503, "无法创建 sudo 输入管道")
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return httpapi.NewError(503, "无法创建删除进度管道")
	}
	defer output.Close()
	prompt := &sudoCleanupInput{input: input, password: password}
	cmd.Stderr = prompt
	defer prompt.clear()
	cmd.Cancel = func() error { _ = input.Close(); return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	if err = cmd.Start(); err != nil {
		return httpapi.NewError(503, "无法启动 sudo，请检查安装和服务账号的 sudo 权限")
	}
	waited := false
	defer func() {
		_ = input.Close()
		if !waited {
			cancel()
			_ = cmd.Wait()
		}
	}()
	authTimer := time.AfterFunc(30*time.Second, cancel)
	defer authTimer.Stop()
	scanner := bufio.NewScanner(output)
	// A failure includes both the selected path and the failing child path;
	// JSON escaping can expand each byte of a Linux path up to six characters.
	scanner.Buffer(make([]byte, 4096), 64*1024)
	read := func() (cleanupHelperEvent, error) {
		var event cleanupHelperEvent
		if !scanner.Scan() {
			return event, fmt.Errorf("删除辅助程序未返回完整结果")
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return event, fmt.Errorf("删除辅助程序返回无效结果")
		}
		return event, nil
	}
	ready, err := read()
	if err != nil || ready.Type != "ready" {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return httpapi.NewError(403, "sudo 认证失败或无执行权限，请重新输入服务账号的 sudo 密码；密码未保存")
	}
	prompt.clear()
	authTimer.Stop()
	if err := childCtx.Err(); err != nil {
		return err
	}
	if err = json.NewEncoder(input).Encode(request); err != nil {
		return fmt.Errorf("无法发送本次删除范围")
	}
	pending := map[string]bool{}
	for _, path := range request.Paths {
		pending[path] = true
	}
	for len(pending) > 0 {
		event, err := read()
		if err != nil {
			return err
		}
		if event.Type != "result" || !pending[event.Path] || (event.Status != "deleted" && event.Status != "failed") {
			return fmt.Errorf("删除进度与本次所选路径不匹配")
		}
		message := ""
		if event.Code != "remove_failed" && event.Code != "preflight_failed" && event.Error != "" {
			return fmt.Errorf("删除辅助程序返回矛盾状态")
		}
		if event.Status == "failed" {
			switch event.Code {
			case "preflight_failed":
				if event.Error == "" {
					return fmt.Errorf("删除辅助程序未返回检查失败原因")
				}
				message = "删除前检查失败：" + event.Error + "；本批未执行删除"
			case "cancelled":
				message = "删除已停止，可能已删除部分内容，请检查实际路径"
			case "remove_failed":
				if event.Error == "" {
					return fmt.Errorf("删除辅助程序未返回失败原因")
				}
				message = "删除失败：" + event.Error + "；可能已删除部分内容，请检查实际路径"
			default:
				return fmt.Errorf("删除辅助程序返回未知状态")
			}
		} else if event.Code != "" {
			return fmt.Errorf("删除辅助程序返回矛盾状态")
		}
		if err := result(event.Path, event.Status, message); err != nil {
			return err
		}
		delete(pending, event.Path)
	}
	done, err := read()
	if err != nil || done.Type != "done" {
		return fmt.Errorf("删除辅助程序未正常完成")
	}
	err = cmd.Wait()
	waited = true
	if err != nil {
		return fmt.Errorf("删除辅助程序异常退出，请核对逐项结果")
	}
	return ctx.Err()
}

// This mode is started only for a single sudo command. It never opens the
// platform database, a model connection, or a log file, and never sees a password.
func ServeCleanupHelper(ctx context.Context, input io.Reader, output io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("删除辅助程序需要 sudo 权限")
	}
	return serveCleanupHelper(ctx, input, output)
}
func serveCleanupHelper(parent context.Context, input io.Reader, output io.Writer) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	encoder := json.NewEncoder(output)
	if err := encoder.Encode(cleanupHelperEvent{Type: "ready"}); err != nil {
		return err
	}
	reader := bufio.NewReader(io.LimitReader(input, 2*1024*1024))
	raw, err := reader.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("缺少本次删除范围")
	}
	var request cleanupHelperRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || len(request.Paths) < 1 || len(request.Paths) > 100 {
		return fmt.Errorf("删除范围格式无效")
	}
	// Losing the service pipe cancels root work even if the service crashes or the
	// unprivileged process cannot signal its privileged child directly.
	go func() { _, _ = io.Copy(io.Discard, reader); cancel() }()
	mountFile, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("无法核对挂载点")
	}
	mounts, err := readCleanupMounts(mountFile)
	mountFile.Close()
	if err != nil {
		return fmt.Errorf("无法核对挂载点")
	}
	targets, preflightErr := prepareCleanupTargets(request, mounts)
	if preflightErr == nil {
		defer closeCleanupTargets(targets)
	}
	for i, path := range request.Paths {
		event := cleanupHelperEvent{Type: "result", Path: path, Status: "deleted"}
		if preflightErr != nil {
			event.Status, event.Code, event.Error = "failed", "preflight_failed", preflightErr.Error()
		} else if err := targets[i].remove(ctx); err != nil {
			event.Status, event.Code = "failed", "remove_failed"
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				event.Code = "cancelled"
			} else {
				// Only filesystem diagnostics from this helper, never sudo/PAM
				// stderr or authentication input, enter the persisted result.
				event.Error = err.Error()
			}
		}
		if err := encoder.Encode(event); err != nil {
			return fmt.Errorf("删除进度连接已关闭")
		}
	}
	return encoder.Encode(cleanupHelperEvent{Type: "done"})
}
