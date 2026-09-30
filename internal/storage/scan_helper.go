package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"project-alpha/internal/fsutil"
	"project-alpha/internal/platform"
)

const helperLabel = "project-alpha.scan-helper"
const helperHeartbeatTimeout = 10 * time.Second

type helperLeaseKey struct{}
type helperRequest struct {
	StreamDirectory bool            `json:"stream_directory,omitempty"`
	Seed            []InodeRecord   `json:"seed,omitempty"`
	RetainPaths     []string        `json:"retain_paths,omitempty"`
	StrictPaths     bool            `json:"strict_paths,omitempty"`
	Containers      []scanContainer `json:"containers,omitempty"`
	Resources       []Resource      `json:"resources,omitempty"`
	AnalyzeFiles    bool            `json:"analyze_files,omitempty"`
	Version         int             `json:"version"`
	Config          Config          `json:"config"`
	Paths           []string        `json:"paths"`
	WritablePaths   []string        `json:"writable_paths"`
	Mounts          []MountInfo     `json:"mounts"`
}
type physicalScan struct {
	Accounting        []InodeRecord     `json:"accounting,omitempty"`
	Analysis          *FileAnalysis     `json:"analysis,omitempty"`
	Tree              *Node             `json:"tree"`
	Filesystems       []object          `json:"filesystems"`
	Warnings          []Warning         `json:"warnings"`
	Nodes             int64             `json:"nodes"`
	Visited           int64             `json:"visited"`
	ErrorCount        int64             `json:"error_count"`
	OmittedReferences int64             `json:"omitted_references"`
	Excludes          []string          `json:"excludes"`
	CanonicalPaths    map[string]string `json:"canonical_paths"`
	EUID              int               `json:"euid"`
	Backend           string            `json:"backend"`
}
type helperEvent struct {
	Progress        object        `json:"progress,omitempty"`
	DirectoryUpdate *Node         `json:"directory_update,omitempty"`
	Result          *physicalScan `json:"result,omitempty"`
}

// Empty controls are heartbeats. Only the receiver of the complete result may
// acknowledge it: older Docker CLIs truncate pending stdout on container exit.
type helperControl struct {
	ResultReceived bool `json:"result_received,omitempty"`
}

func scanPhysical(ctx context.Context, request helperRequest, progress func(object) error, readOnly bool) (*physicalScan, error) {
	if request.StrictPaths {
		for _, p := range request.Paths {
			if fsutil.Canonical(p) != p {
				return nil, fmt.Errorf("扫描路径已改变，请重新扫描：%s", p)
			}
		}
	}
	s := newScanner(request.Config, request.Mounts, progress)
	s.StreamDirectory = request.StreamDirectory
	// Ordinary scans only retain their budgeted display tree. Identity records
	// are collected on demand, and only for retained inspection nodes.
	if request.StrictPaths {
		s.Ledger = map[string]InodeRecord{}
	}
	for _, r := range request.Seed {
		s.seen[inode{r.Device, r.Inode}] = r.Path
		s.retained[r.Path] = true
	}
	for _, p := range request.RetainPaths {
		for {
			s.required[p] = true
			if p == filepath.Dir(p) {
				break
			}
			p = filepath.Dir(p)
		}
	}
	s.ContainerProgress = newContainerProgress(request.Containers, request.Resources)
	if request.AnalyzeFiles {
		s.Analysis = newFileAnalysis()
	}
	s.RequireReadOnly = readOnly
	s.DetailRoots = map[string]bool{}
	for _, p := range request.WritablePaths {
		s.DetailRoots[fsutil.Canonical(p)] = true
	}
	paths := map[string]string{}
	for _, p := range request.Paths {
		paths[p] = fsutil.Canonical(p)
	}
	tree, err := s.Scan(ctx, request.Paths)
	if err != nil {
		return nil, err
	}
	filesystems := s.filesystems()
	if err := s.report("", true); err != nil {
		return nil, err
	}
	if s.Analysis != nil {
		s.Analysis.finish()
	}
	backend := "host"
	if readOnly {
		backend = "docker"
	}
	var ledger []InodeRecord
	if s.Ledger != nil {
		ledger = []InodeRecord{}
		for _, r := range s.Ledger {
			ledger = append(ledger, r)
		}
	}
	return &physicalScan{Accounting: ledger, Tree: tree, Analysis: s.Analysis, Filesystems: filesystems, Warnings: s.Errors, Nodes: s.Nodes, Visited: s.Visited, ErrorCount: s.ErrorCount, OmittedReferences: s.OmittedReferences, Excludes: s.Config.Exclude, CanonicalPaths: paths, EUID: os.Geteuid(), Backend: backend}, nil
}

// stdin stays open for heartbeats. If the client, worker or service disappears,
// the helper stops itself even though Docker containers are not worker children.
func watchHelperInput(ctx context.Context, decoder *json.Decoder, cancel context.CancelFunc, timeout time.Duration, received chan struct{}) {
	heartbeat := make(chan struct{}, 1)
	go func() {
		for {
			var beat helperControl
			if decoder.Decode(&beat) != nil {
				cancel()
				return
			}
			if beat.ResultReceived {
				close(received)
				return
			}
			select {
			case heartbeat <- struct{}{}:
			default:
			}
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-received:
			return
		case <-timer.C:
			cancel()
			return
		case <-heartbeat:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)
		}
	}
}

func ServeScanHelper(ctx context.Context, input io.Reader, output io.Writer) error {
	decoder := json.NewDecoder(input)
	var request helperRequest
	type decodedRequest struct {
		request helperRequest
		err     error
	}
	initial := make(chan decodedRequest, 1)
	go func() { var r helperRequest; err := decoder.Decode(&r); initial <- decodedRequest{r, err} }()
	timer := time.NewTimer(helperHeartbeatTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("helper request timed out")
	case r := <-initial:
		if r.err != nil {
			return fmt.Errorf("helper request: %w", r.err)
		}
		request = r.request
	}
	if request.Version != 1 || request.Config.MaxDepth < 0 || request.Config.MaxDepth > 32 || request.Config.MaxNodes < 1 || request.Config.MaxNodes > maxScanNodes || len(request.Paths) == 0 {
		return fmt.Errorf("invalid helper request")
	}
	for _, p := range append(append([]string{}, request.Paths...), request.Config.Exclude...) {
		if !filepath.IsAbs(p) || strings.ContainsRune(p, 0) {
			return fmt.Errorf("helper paths must be absolute")
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	received := make(chan struct{})
	go watchHelperInput(ctx, decoder, cancel, helperHeartbeatTimeout, received)
	encoder := json.NewEncoder(output)
	progress := func(v object) error {
		event := helperEvent{Progress: v}
		if value, ok := v["directory_update"]; ok {
			var valid bool
			event.DirectoryUpdate, valid = value.(*Node)
			if !valid || event.DirectoryUpdate == nil {
				return fmt.Errorf("invalid directory update")
			}
			delete(v, "directory_update")
		}
		return encoder.Encode(event)
	}
	// The helper itself adds an overlay mount after host-side discovery. Read
	// mountinfo again inside the chroot so its merged view is excluded too.
	request.Mounts = append(request.Mounts, mountTable()...)
	if err := progress(object{"phase": "preparing", "path": "只读辅助容器已就绪", "entries": 0, "preparation_done": 2, "preparation_total": 2}); err != nil {
		return err
	}
	result, err := scanPhysical(ctx, request, progress, true)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := encoder.Encode(helperEvent{Result: result}); err != nil {
		return err
	}
	// A successful write only reaches Docker's buffers. Stay alive until the
	// client has decoded the result; heartbeats cover slow result consumers.
	select {
	case <-received:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func helperRunArgs(imageID, executable, token string, c Config) []string {
	parallelism := c.scanParallelism()
	return []string{"run", "--rm", "--pull", "never", "--name", "project-alpha-scan-" + token,
		"--label", helperLabel + "=" + token, "--interactive", "--sig-proxy=false", "--read-only", "--network", "none",
		"--user", "0:0", "--cap-drop", "ALL", "--cap-add", "DAC_READ_SEARCH", "--cap-add", "SYS_CHROOT",
		"--security-opt", "no-new-privileges", "--log-driver", "none", "--pids-limit", strconv.Itoa(helperPIDLimit(parallelism)),
		// The PID limit also counts threads. Set this before the Go runtime starts:
		// on large hosts its default CPU/GC parallelism can exhaust the limit.
		"--env", "GOMAXPROCS=" + strconv.Itoa(parallelism),
		// Docker requires rslave when a bind contains DockerRootDir. On supported
		// kernels readonly is recursive; the scanner also rejects writable dirs.
		"--mount", "type=bind,src=/,dst=/host,readonly,bind-propagation=rslave",
		"--entrypoint", "/usr/sbin/chroot", imageID, "/host", executable, "scan-helper"}
}

var helperTokenPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var helperImagePattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Remove by ID only after matching the label. A name collision must never
// allow us to delete a pre-existing container belonging to somebody else.
func cleanupScanHelper(token string, command dockerCommand) error {
	if !helperTokenPattern.MatchString(token) {
		return fmt.Errorf("invalid helper lease")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := command(ctx, []string{"container", "inspect", "--format", `{{.Id}} {{index .Config.Labels "` + helperLabel + `"}}`, "project-alpha-scan-" + token}, 5)
	if err != nil {
		if strings.Contains(err.Error(), "No such") {
			return nil
		}
		return err
	}
	fields := strings.Fields(raw)
	if len(fields) != 2 || fields[1] != token || !containerPattern.MatchString(fields[0]) {
		return fmt.Errorf("helper ownership could not be verified; container left untouched")
	}
	_, err = command(ctx, []string{"container", "rm", "--force", fields[0]}, 5)
	if err != nil && strings.Contains(err.Error(), "No such") {
		return nil
	}
	return err
}

func cleanupHelperLease(path string) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var token string
	if err = json.Unmarshal(raw, &token); err != nil {
		return err
	}
	if err = cleanupScanHelper(token, runDocker); err != nil {
		return err
	}
	return os.Remove(path)
}

const defaultHelperImage = "alpine:latest"

// A first pull is a network operation and may outlast the per-command Docker
// timeout, so it gets its own budget. It stays cancellable through ctx.
const helperPullTimeout = 600

// dockerImageMissing reports whether an image inspect failed because the image is
// absent locally. Only that cause may be answered with a pull: an unreachable
// daemon, a denied socket or a missing Docker client fails inspect for a different
// reason, and pulling would fail the same way while dressing the real cause up as
// a missing image.
func dockerImageMissing(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "no such image")
}

func validateHelperImage(raw string) (string, error) {
	if id := strings.TrimSpace(raw); helperImagePattern.MatchString(id) {
		return id, nil
	}
	return "", fmt.Errorf("invalid helper image ID")
}

// resolveHelperImage returns the local ID of the read-only scan image, pulling it
// once when it is absent. A missing image fails the scan instead of degrading to
// a host scan: a non-root service user cannot read paths such as /var/lib/docker,
// so that fallback silently under-reports disk usage.
func resolveHelperImage(ctx context.Context, imageName string, inspectTimeout int, progress func(object) error) (string, error) {
	raw, err := runDocker(ctx, []string{"image", "inspect", "--format", "{{.Id}}", imageName}, inspectTimeout)
	if err == nil {
		return validateHelperImage(raw)
	}
	if !dockerImageMissing(err) {
		return "", fmt.Errorf("无法通过 Docker 确认只读扫描镜像 %s（可用 PROJECT_ALPHA_SCAN_HELPER_IMAGE 指定已有镜像）：%w", imageName, err)
	}
	if progress != nil {
		if err := progress(object{"phase": "preparing", "path": "本机不存在只读扫描镜像，正在拉取 " + imageName, "entries": 0, "preparation_done": 0, "preparation_total": 2}); err != nil {
			return "", err
		}
	}
	if _, err = runDocker(ctx, []string{"pull", imageName}, helperPullTimeout); err != nil {
		return "", fmt.Errorf("只读扫描需要镜像 %s，本机不存在且拉取失败（可用 PROJECT_ALPHA_SCAN_HELPER_IMAGE 指定已有镜像）：%w", imageName, err)
	}
	raw, err = runDocker(ctx, []string{"image", "inspect", "--format", "{{.Id}}", imageName}, inspectTimeout)
	if err != nil {
		return "", fmt.Errorf("只读扫描镜像 %s 拉取后仍无法确认：%w", imageName, err)
	}
	return validateHelperImage(raw)
}

// scanViaDocker runs the read-only helper container and returns the scan it
// produced. Failures are returned as they are: the caller does not fall back to a
// host scan, because a non-root service user cannot fully read the same paths.
func scanViaDocker(ctx context.Context, request helperRequest, progress func(object) error) (result *physicalScan, err error) {
	imageName := os.Getenv("PROJECT_ALPHA_SCAN_HELPER_IMAGE")
	if imageName == "" {
		imageName = defaultHelperImage
	}
	imageID, err := resolveHelperImage(ctx, imageName, request.Config.DockerTimeout, progress)
	if err != nil {
		return nil, err
	}
	if progress != nil {
		if err := progress(object{"phase": "preparing", "path": "扫描镜像已确认，正在启动只读辅助容器", "preparation_done": 1, "preparation_total": 2}); err != nil {
			return nil, err
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(executable, " (deleted)") {
		return nil, fmt.Errorf("扫描程序已被替换，请重启服务后再扫描")
	}
	token := platform.RandomHex(16)
	lease, _ := ctx.Value(helperLeaseKey{}).(string)
	if lease != "" {
		if err := atomicWrite(lease, token); err != nil {
			return nil, err
		}
	}
	defer func() {
		cleanupErr := cleanupScanHelper(token, runDocker)
		if cleanupErr == nil && lease != "" {
			cleanupErr = os.Remove(lease)
			if os.IsNotExist(cleanupErr) {
				cleanupErr = nil
			}
		}
		if cleanupErr != nil {
			if err == nil {
				err = fmt.Errorf("辅助扫描容器清理失败：%w", cleanupErr)
			}
		}
	}()
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(childCtx, "docker", helperRunArgs(imageID, executable, token, request.Config)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	defer stdin.Close()
	// Bound startup independently of the potentially hours-long filesystem scan.
	startup := time.AfterFunc(time.Duration(request.Config.DockerTimeout)*time.Second, cancel)
	defer startup.Stop()
	var heartbeat sync.WaitGroup
	received := make(chan struct{})
	heartbeat.Add(1)
	go func() {
		defer heartbeat.Done()
		encoder := json.NewEncoder(stdin)
		if encoder.Encode(request) != nil {
			cancel()
			return
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-childCtx.Done():
				return
			case <-received:
				// Keep all stdin writes in this goroutine, so an acknowledgment
				// cannot interleave with a heartbeat or be followed by one.
				if encoder.Encode(helperControl{ResultReceived: true}) != nil {
					cancel()
				}
				return
			case <-ticker.C:
				if encoder.Encode(helperControl{}) != nil {
					cancel()
					return
				}
			}
		}
	}()
	decoder := json.NewDecoder(stdout)
	decoder.UseNumber()
	var decodeErr error
	for {
		var event helperEvent
		if decodeErr = decoder.Decode(&event); decodeErr != nil {
			break
		}
		// Decode directory trees directly into Nodes. Passing them through
		// map[string]any would allocate every field and then require another
		// JSON encode/decode before each incremental publication.
		if event.DirectoryUpdate != nil {
			if event.Progress == nil {
				decodeErr = fmt.Errorf("helper directory update has no progress")
				break
			}
			event.Progress["directory_update"] = event.DirectoryUpdate
		}
		if event.Progress != nil {
			startup.Stop()
			if progress != nil {
				if decodeErr = progress(event.Progress); decodeErr != nil {
					break
				}
			}
		}
		if event.Result != nil {
			if result != nil {
				decodeErr = fmt.Errorf("duplicate helper result")
				break
			}
			if event.Result.Tree == nil || event.Result.Backend != "docker" {
				decodeErr = fmt.Errorf("helper did not return a complete scan")
				break
			}
			result = event.Result
			close(received)
		}
	}
	if decodeErr != io.EOF {
		cancel()
	}
	waitErr := cmd.Wait()
	cancel()
	stdin.Close()
	heartbeat.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if decodeErr != nil && decodeErr != io.EOF {
		return nil, fmt.Errorf("helper stream: %w", decodeErr)
	}
	if waitErr != nil {
		return nil, fmt.Errorf("只读辅助扫描失败：%w: %s", waitErr, stderr.String())
	}
	if result == nil || result.Tree == nil || result.Backend != "docker" {
		return nil, fmt.Errorf("helper did not return a complete scan")
	}
	return result, nil
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 8192 - b.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.Buffer.Write(p)
	}
	return n, nil
}

// collectRequest scans the requested paths. Docker-enabled non-root scans always
// use the read-only helper: a missing image, a failed pull or a helper that will
// not start fails the task instead of silently falling back to a host scan the
// service user cannot fully read. Choose the "host" backend to scan without Docker.
func collectRequest(ctx context.Context, request helperRequest, progress func(object) error) (*physicalScan, error) {
	c, paths := request.Config, request.Paths
	useDocker := !c.NoDocker && len(paths) > 0 && (c.ScanBackend == "docker" || (c.ScanBackend == "auto" && os.Geteuid() != 0))
	if !useDocker {
		return scanPhysical(ctx, request, progress, false)
	}
	if progress != nil {
		if err := progress(object{"phase": "preparing", "path": "正在确认只读扫描镜像", "entries": 0, "preparation_done": 0, "preparation_total": 2}); err != nil {
			return nil, err
		}
	}
	return scanViaDocker(ctx, request, progress)
}
