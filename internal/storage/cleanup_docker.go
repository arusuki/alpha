package storage

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

//go:embed cleanup_container.py
var cleanupContainerProgram string

var cleanupContainerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

type cleanupCommand func(context.Context, string, ...string) *exec.Cmd

// Always address the local daemon explicitly; physical paths must never be
// matched against a daemon selected by a user's Docker context/environment.
var cleanupDockerArgs = []string{"--host", "unix:///var/run/docker.sock"}

const cleanupInspectFormat = `{"id":{{json .Id}},"upper":{{json .GraphDriver.Data.UpperDir}},"running":{{json .State.Running}},"paused":{{json .State.Paused}},"restarting":{{json .State.Restarting}},"readonly":{{json .HostConfig.ReadonlyRootfs}},"mounts":{{json .Mounts}}}`

type cleanupDockerSession struct {
	cmd     *exec.Cmd
	input   io.WriteCloser
	output  io.ReadCloser
	scanner *bufio.Scanner
	cancel  context.CancelFunc
	stderr  bytes.Buffer
	paths   []string
	closed  bool
}

type cleanupDockerEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Error string `json:"error"`
	cleanupStats
}

func startCleanupDocker(ctx context.Context, c cleanupContainer, paths, protected []string, command cleanupCommand) (*cleanupDockerSession, error) {
	protected = append([]string{}, protected...)
	inspectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	cmd := command(inspectCtx, "docker", append(append([]string{}, cleanupDockerArgs...), "inspect", "--type", "container", "--format", cleanupInspectFormat, c.ID)...)
	raw, err := cmd.Output()
	cancel()
	if err != nil {
		return nil, fmt.Errorf("无法核对容器 %s：%w", c.ID, err)
	}
	var current struct {
		ID         string `json:"id"`
		Upper      string `json:"upper"`
		Running    bool   `json:"running"`
		Paused     bool   `json:"paused"`
		Restarting bool   `json:"restarting"`
		Readonly   bool   `json:"readonly"`
		Mounts     []struct{ Destination string }
	}
	if json.Unmarshal(raw, &current) != nil || current.ID != c.ID || current.Upper != c.Upper {
		return nil, fmt.Errorf("容器身份或可写层已变化，请重新扫描：%s", c.ID)
	}
	if !current.Running || current.Paused || current.Restarting {
		return nil, fmt.Errorf("容器未运行、已暂停或正在重启：%s", c.ID)
	}
	if current.Readonly {
		return nil, fmt.Errorf("容器根文件系统只读：%s", c.ID)
	}
	for _, m := range current.Mounts {
		protected = append(protected, m.Destination)
	}
	childCtx, stop := context.WithCancel(ctx)
	session := &cleanupDockerSession{cancel: stop, paths: paths}
	args := append(append([]string{}, cleanupDockerArgs...), "exec", "--interactive", "--user", "0:0", "--workdir", "/", c.ID, "python3", "-I", "-S", "-u", "-c", cleanupContainerProgram)
	session.cmd = command(childCtx, "docker", args...)
	session.cmd.Stderr = &session.stderr
	session.input, err = session.cmd.StdinPipe()
	if err != nil {
		stop()
		return nil, err
	}
	session.output, err = session.cmd.StdoutPipe()
	if err != nil {
		session.input.Close()
		stop()
		return nil, err
	}
	// Killing the Docker CLI alone can leave the exec process alive. First close
	// stdin: the container worker exits on EOF even while it is traversing files.
	session.cmd.Cancel = func() error { return session.input.Close() }
	session.cmd.WaitDelay = 3 * time.Second
	if err = session.cmd.Start(); err != nil {
		session.input.Close()
		session.output.Close()
		stop()
		return nil, err
	}
	session.scanner = bufio.NewScanner(session.output)
	session.scanner.Buffer(make([]byte, 4096), 64*1024)
	timer := time.AfterFunc(20*time.Second, stop)
	defer timer.Stop()
	if err = json.NewEncoder(session.input).Encode(struct {
		Upper     string   `json:"upper"`
		Paths     []string `json:"paths"`
		Protected []string `json:"protected"`
	}{c.Upper, paths, protected}); err == nil {
		var event cleanupDockerEvent
		event, err = session.read()
		if err == nil && event.Type != "prepared" {
			err = fmt.Errorf("容器内清理检查失败：%s", event.Error)
		}
	}
	if err == nil {
		err = childCtx.Err()
	}
	if err != nil {
		session.close()
		detail := strings.TrimSpace(session.stderr.String())
		if len(detail) > 2048 {
			detail = detail[:2048]
		}
		return nil, fmt.Errorf("docker exec 清理预检失败（容器需安装 Python 3，支持 ctypes/openat2）：%w；%s", err, detail)
	}
	return session, nil
}

func (s *cleanupDockerSession) read() (cleanupDockerEvent, error) {
	var e cleanupDockerEvent
	if !s.scanner.Scan() {
		return e, fmt.Errorf("容器内清理进程未返回完整结果")
	}
	if json.Unmarshal(s.scanner.Bytes(), &e) != nil || e.Sockets < 0 || e.CharDevices < 0 {
		return e, fmt.Errorf("容器内清理进程返回无效结果")
	}
	return e, nil
}
func (s *cleanupDockerSession) remove(index int, stats *cleanupStats) error {
	if err := json.NewEncoder(s.input).Encode(struct {
		Index int `json:"index"`
	}{index}); err != nil {
		return err
	}
	e, err := s.read()
	if err != nil {
		return err
	}
	if e.Type != "result" || e.Index != index {
		return fmt.Errorf("容器内清理结果与目标不匹配")
	}
	*stats = e.cleanupStats
	if e.Error != "" {
		return fmt.Errorf("容器内路径 %s：%s", s.paths[index], e.Error)
	}
	return nil
}
func (s *cleanupDockerSession) close() {
	if s.closed {
		return
	}
	s.closed = true
	s.input.Close()
	s.cancel()
	s.cmd.Wait()
	s.output.Close()
}
