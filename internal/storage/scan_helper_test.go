package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
)

func TestHelperHeartbeatDisconnectAndTimeout(t *testing.T) {
	for _, disconnect := range []bool{true, false} {
		reader, writer := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		go watchHelperInput(ctx, json.NewDecoder(reader), cancel, 80*time.Millisecond, make(chan struct{}))
		if _, err := writer.Write([]byte("{}\n")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("live helper cancelled")
		case <-time.After(20 * time.Millisecond):
		}
		if disconnect {
			writer.Close()
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("orphan helper kept scanning")
		}
		reader.Close()
		writer.Close()
		cancel()
	}
}

func TestHelperWaitsForResultAcknowledgment(t *testing.T) {
	for _, finish := range []string{"ack", "disconnect", "cancel"} {
		t.Run(finish, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			input, control := io.Pipe()
			output, stream := io.Pipe()
			defer input.Close()
			defer control.Close()
			defer output.Close()
			defer stream.Close()
			done := make(chan error, 1)
			go func() { done <- ServeScanHelper(ctx, input, stream) }()
			encoder := json.NewEncoder(control)
			request := helperRequest{Version: 1, Config: defaultConfig(), Paths: []string{t.TempDir()}}
			if err := encoder.Encode(request); err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(output)
			for {
				var event helperEvent
				if err := decoder.Decode(&event); err != nil {
					t.Fatal(err)
				}
				if event.Result != nil {
					break
				}
			}
			// The result is fully transmitted, but ordinary heartbeats must not
			// release the helper before the receiver explicitly acknowledges it.
			if err := encoder.Encode(helperControl{}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				t.Fatalf("helper exited before acknowledgment: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			switch finish {
			case "ack":
				if err := encoder.Encode(helperControl{ResultReceived: true}); err != nil {
					t.Fatal(err)
				}
			case "disconnect":
				control.Close()
			case "cancel":
				cancel()
			}
			select {
			case err := <-done:
				if finish == "ack" && err != nil || finish != "ack" && !errors.Is(err, context.Canceled) {
					t.Fatalf("finish=%s err=%v", finish, err)
				}
			case <-time.After(time.Second):
				t.Fatal("helper did not stop")
			}
		})
	}
}

func TestHelperCleanupOnlyRemovesOwnedContainerID(t *testing.T) {
	token := strings.Repeat("a", 32)
	id := strings.Repeat("b", 64)
	for _, label := range []string{token, "somebody-else"} {
		removed := ""
		command := func(_ context.Context, args []string, _ int) (string, error) {
			if args[1] == "inspect" {
				return id + " " + label, nil
			}
			if args[1] != "rm" || len(args) != 4 || args[2] != "--force" {
				t.Fatalf("unexpected cleanup: %v", args)
			}
			removed = args[3]
			return id, nil
		}
		err := cleanupScanHelper(token, command)
		if label == token && (err != nil || removed != id) {
			t.Fatalf("owned helper not cleaned: %v %s", err, removed)
		}
		if label != token && (err == nil || removed != "") {
			t.Fatal("unrelated container removed")
		}
	}
}

func TestWritableDetailsSurviveWholeDiskBudget(t *testing.T) {
	root := t.TempDir()
	upper := filepath.Join(root, "var", "lib", "docker", "overlay2", "layer", "diff")
	model := filepath.Join(upper, "root", ".cache", "models")
	if err := os.MkdirAll(model, 0700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(model, "weights"), make([]byte, 8192))
	c := defaultConfig()
	c.MaxDepth = 0
	c.MaxNodes = 1
	r, err := scanPhysical(context.Background(), helperRequest{Config: c, Paths: []string{root, upper}, WritablePaths: []string{upper}, Mounts: []MountInfo{}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	index := snapshotNodes(r.Tree)
	if index[model] == nil || index[model].Allocated < 8192 {
		t.Fatal("writable structure was lost under whole disk root")
	}
	if r.Tree.Allocated != scanForTest(t, defaultConfig(), []string{root}).Allocated {
		t.Fatal("reserved layer details changed total")
	}
}

func TestHelperRefusesWritableHostView(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "data"), make([]byte, 8192))
	r, err := scanPhysical(context.Background(), helperRequest{Config: defaultConfig(), Paths: []string{root}, Mounts: []MountInfo{}}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Tree.Files != 0 || r.Tree.Errors != 1 || r.Tree.Children[0].Kind != "unreadable" {
		t.Fatal("helper accepted a writable host mount")
	}
}

func TestWritableDetailsReserveShallowDirectories(t *testing.T) {
	upper := t.TempDir()
	for _, name := range []string{"root", "usr"} {
		if err := os.Mkdir(filepath.Join(upper, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Populate whichever branch the filesystem will visit first, rather than
	// relying on alphabetical ordering of Readdirnames.
	dir, err := os.Open(upper)
	if err != nil {
		t.Fatal(err)
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		p := filepath.Join(upper, names[0], fmt.Sprintf("directory-%04d", i), "nested")
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(p, "file"), make([]byte, 4096))
	}
	last := filepath.Join(upper, names[1], "important")
	if err := os.Mkdir(last, 0700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(last, "weights"), make([]byte, 8192))
	for _, budget := range []int{1, 100000} {
		c := defaultConfig()
		c.MaxDepth, c.MaxNodes = 10, budget
		r, err := scanPhysical(context.Background(), helperRequest{Config: c, Paths: []string{upper}, WritablePaths: []string{upper}, Mounts: []MountInfo{}}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		index := snapshotNodes(r.Tree)
		for _, name := range names {
			if n := index[filepath.Join(upper, name)]; n == nil || n.Allocated < 8192 {
				t.Fatalf("budget %d: top-level %s was consumed by an earlier branch", budget, name)
			}
		}
		if r.Tree.Allocated != scanForTest(t, defaultConfig(), []string{upper}).Allocated {
			t.Fatal("detail quotas changed physical usage")
		}
	}
}

// A fake Docker CLI verifies framing, startup failures and cancellation without
// granting container access to the regular unit test suite.
func installHelperDockerFixture(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	id, image := strings.Repeat("b", 64), "sha256:"+strings.Repeat("c", 64)
	result := &physicalScan{Tree: &Node{Path: "@root", Kind: "root", Children: []*Node{}}, Backend: "docker", CanonicalPaths: map[string]string{}}
	frame := httpapi.JSONText(helperEvent{Result: result})
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
image) echo '%s';;
run)
  for arg in "$@"; do
    case "$arg" in project-alpha.scan-helper=*) token=${arg#*=};; esac
  done
  echo "$token" > '%s/token'
  IFS= read -r request
  echo "$request" > '%s/request'
  case '%s' in
    startup-fail) echo 'cannot mount' >&2; exit 1;;
    malformed) echo '{not json'; exit 1;;
    truncated) echo '{"result":{"tree":'; exit 0;;
    invalid) echo '{"result":{"backend":"docker"}}'; exit 0;;
    missing) echo '{"progress":{"entries":1}}'; exit 0;;
    wait) echo '{"progress":{"entries":1}}'; sleep 60;;
    *) echo '{"progress":{"entries":1}}'; echo '%s'
       while IFS= read -r control; do
         if [ "$control" = '{"result_received":true}' ]; then
           echo "$control" > '%s/ack'
           exit 0
         fi
       done
       exit 1;;
  esac;;
container)
  case "$2" in
    inspect) echo "%s $(cat '%s/token')";;
    rm) echo "$4" > '%s/removed';;
  esac;;
*) exit 1;;
esac
`, image, dir, dir, mode, frame, dir, id, dir, dir)
	mustWrite(t, filepath.Join(dir, "docker"), []byte(script))
	if err := os.Chmod(filepath.Join(dir, "docker"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

func TestDockerHelperProtocolAndCleanup(t *testing.T) {
	for _, mode := range []string{"success", "startup-fail", "malformed", "truncated", "invalid", "missing", "wait"} {
		t.Run(mode, func(t *testing.T) {
			dir := installHelperDockerFixture(t, mode)
			lease := filepath.Join(dir, "lease.json")
			ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), helperLeaseKey{}, lease), 400*time.Millisecond)
			defer cancel()
			request := helperRequest{Version: 1, Config: defaultConfig(), Paths: []string{"/a path with spaces"}, Mounts: []MountInfo{}}
			// progress counts every frame scanViaDocker forwards; helperFrames
			// counts only the ones the container itself produced, which is the
			// signal that the scan actually started.
			progress, helperFrames := 0, 0
			r, err := scanViaDocker(ctx, request, func(v object) error {
				progress++
				if v["entries"] != nil {
					helperFrames++
				}
				return nil
			})
			if mode == "success" && (err != nil || r == nil || progress != 2 || helperFrames != 1) {
				t.Fatalf("valid helper failed: %v %+v", err, r)
			}
			if mode != "success" && (err == nil || r != nil) {
				t.Fatalf("failed helper was accepted: %s", mode)
			}
			if mode == "success" {
				if _, err := os.Stat(filepath.Join(dir, "ack")); err != nil {
					t.Fatalf("complete result was not acknowledged: %v", err)
				}
			} else if _, err := os.Stat(filepath.Join(dir, "ack")); !os.IsNotExist(err) {
				t.Fatal("failed result was acknowledged")
			}
			// A startup failure must never be reported as a running scan: the
			// helper produced no frame before it exited.
			if mode == "startup-fail" && helperFrames != 0 {
				t.Fatal("startup failure was reported as a running scan")
			}
			if mode == "wait" && ctx.Err() != context.DeadlineExceeded {
				t.Fatal("unexpected cancellation")
			}
			raw, readErr := os.ReadFile(filepath.Join(dir, "removed"))
			if readErr != nil || strings.TrimSpace(string(raw)) != strings.Repeat("b", 64) {
				t.Fatalf("helper not cleaned: %v %s", readErr, raw)
			}
			if _, err := os.Stat(lease); !os.IsNotExist(err) {
				t.Fatal("completed lease not removed")
			}
			raw, readErr = os.ReadFile(filepath.Join(dir, "request"))
			var sent helperRequest
			if readErr != nil || json.Unmarshal(raw, &sent) != nil || len(sent.Paths) != 1 || sent.Paths[0] != request.Paths[0] {
				t.Fatal("paths were not transmitted intact")
			}
		})
	}
}

// Exercise the real CLI's container-exit/stdout race, including a consumer pause
// longer than the lease timeout. The regular suite never starts containers.
func TestDockerHelperSlowResultIntegration(t *testing.T) {
	if os.Getenv("PROJECT_ALPHA_TEST_DOCKER_MODES") != "1" {
		t.Skip("set PROJECT_ALPHA_TEST_DOCKER_MODES=1 to test real read-only containers")
	}
	root := t.TempDir()
	const files = 512
	for i := 0; i < files; i++ {
		mustWrite(t, filepath.Join(root, fmt.Sprintf("%04d-%s", i, strings.Repeat("x", 96))), nil)
	}
	lease := filepath.Join(t.TempDir(), "lease.json")
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), helperLeaseKey{}, lease), 45*time.Second)
	defer cancel()
	c := defaultConfig()
	c.MaxDepth = 1
	paused := false
	result, err := scanViaDocker(ctx, helperRequest{Version: 1, Config: c, Paths: []string{root}, Mounts: mountTable()}, func(progress object) error {
		if paused || numberInt64(progress["preparation_done"]) != 2 {
			return nil
		}
		paused = true
		timer := time.NewTimer(helperHeartbeatTimeout + 2*time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		raw, err := os.ReadFile(lease)
		if err != nil {
			return err
		}
		var token string
		if err := json.Unmarshal(raw, &token); err != nil {
			return err
		}
		state, err := runDocker(ctx, []string{"container", "inspect", "--format", "{{.State.Running}}", "project-alpha-scan-" + token}, 5)
		if err != nil || strings.TrimSpace(state) != "true" {
			return fmt.Errorf("helper exited before result was consumed: state=%s err=%v", state, err)
		}
		return nil
	})
	if err != nil || !paused || result == nil {
		t.Fatalf("slow receiver failed: paused=%v err=%v", paused, err)
	}
	if result.ErrorCount != 0 || result.Tree.Files != files || len(result.Tree.Children) != 1 || len(result.Tree.Children[0].Children) != files {
		t.Fatalf("incomplete result: errors=%d files=%d", result.ErrorCount, result.Tree.Files)
	}
	if _, err := os.Stat(lease); !os.IsNotExist(err) {
		t.Fatalf("acknowledged helper was not cleaned up: %v", err)
	}
	t.Logf("received all %d files after a %s pause", files, helperHeartbeatTimeout+2*time.Second)
}

func TestHelperMountArguments(t *testing.T) {
	// A large host setting must not leak into the helper's 128-task cgroup.
	t.Setenv("GOMAXPROCS", "128")
	image := "sha256:" + strings.Repeat("a", 64)
	args := helperRunArgs(image, "/path with spaces/scanner", strings.Repeat("b", 32), defaultConfig())
	joined := strings.Join(args, " ")
	for _, required := range []string{"--read-only", "--network none", "--cap-drop ALL", "readonly,bind-propagation=rslave", "--pull never", "--rm", "--sig-proxy=false", "--pids-limit 128"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("helper missing %s", required)
		}
	}
	if strings.Contains(joined, "--privileged") || args[len(args)-2] != "/path with spaces/scanner" {
		t.Fatal("unsafe helper arguments")
	}
	// Docker options must precede the image, and the value must be explicit so
	// Docker does not inherit the service's potentially much larger GOMAXPROCS.
	env := slices.Index(args, "--env")
	if env < 0 || env+1 >= slices.Index(args, image) || args[env+1] != "GOMAXPROCS="+strconv.Itoa(min(4, runtime.NumCPU())) {
		t.Fatalf("helper runtime parallelism is not bounded at startup: %v", args)
	}
	c := defaultConfig()
	c.ScanMode = "fast"
	args = helperRunArgs(image, "/path with spaces/scanner", strings.Repeat("b", 32), c)
	env = slices.Index(args, "--env")
	pids := slices.Index(args, "--pids-limit")
	if env < 0 || env+1 >= slices.Index(args, image) || args[env+1] != "GOMAXPROCS="+strconv.Itoa(runtime.NumCPU()) {
		t.Fatalf("fast mode did not use available CPUs: %v", args)
	}
	if pids < 0 || pids+1 >= slices.Index(args, image) || args[pids+1] != strconv.Itoa(helperPIDLimit(runtime.NumCPU())) {
		t.Fatalf("fast mode did not reserve runtime threads: %v", args)
	}
}

func TestMountedImageViewIsNotCountedAlongsideBackingFile(t *testing.T) {
	root := t.TempDir()
	view := filepath.Join(root, "snap", "package", "1")
	if err := os.MkdirAll(view, 0700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "package.snap"), make([]byte, 8192))
	mustWrite(t, filepath.Join(view, "expanded"), make([]byte, 32768))
	s := newScanner(defaultConfig(), []MountInfo{{Path: view, FS: "squashfs"}}, nil)
	tree, err := s.Scan(context.Background(), []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if tree.Files != 1 || snapshotNodes(tree)[view].Kind != "excluded" {
		t.Fatal("mounted image was charged a second time")
	}
}

// installMissingHelperImageDocker installs a fake CLI where the helper image is
// absent: image inspect fails the way the real daemon reports a missing image and
// image pull exits according to pullFails. Every invocation is appended to the
// returned log file.
func installMissingHelperImageDocker(t *testing.T, pullFails bool) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	pullExit := "0"
	if pullFails {
		pullExit = "1"
	}
	script := fmt.Sprintf(`#!/bin/sh
echo "$1" >> '%s'
case "$1" in
  image) echo "Error response from daemon: No such image: $5" >&2; exit 1;;
  pull) exit %s;;
  *) exit 1;;
esac
`, calls, pullExit)
	mustWrite(t, filepath.Join(dir, "docker"), []byte(script))
	if err := os.Chmod(filepath.Join(dir, "docker"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return calls
}

// The helper image is pulled once when absent. A failed pull must fail the scan
// rather than silently degrade to an unprivileged host scan.
func TestHelperImagePulledOnMissing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pullFails bool
		want      string
	}{
		{"pull fails", true, "拉取失败"},
		{"pull succeeds but image still missing", false, "拉取后仍无法确认"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := installMissingHelperImageDocker(t, tc.pullFails)
			var frames []object
			_, err := resolveHelperImage(context.Background(), defaultHelperImage, 5, func(v object) error {
				frames = append(frames, v)
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("missing image was accepted: %v", err)
			}
			// A pull can be slow, so it must announce itself to the progress UI.
			if len(frames) != 1 || frames[0]["phase"] != "preparing" || !strings.Contains(fmt.Sprint(frames[0]["path"]), "正在拉取") {
				t.Fatalf("pull progress was not reported: %#v", frames)
			}
			raw, readErr := os.ReadFile(calls)
			if readErr != nil || strings.Count(string(raw), "pull") != 1 {
				t.Fatalf("expected exactly one pull attempt: %v %q", readErr, raw)
			}
		})
	}
}

// installBrokenDocker installs a fake CLI whose daemon is unreachable, the way a
// stopped service or a denied socket reports itself.
func installBrokenDocker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := fmt.Sprintf(`#!/bin/sh
echo "$1" >> '%s'
echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?" >&2
exit 1
`, calls)
	mustWrite(t, filepath.Join(dir, "docker"), []byte(script))
	if err := os.Chmod(filepath.Join(dir, "docker"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return calls
}

// An unreachable daemon must be reported as such, not mistaken for a missing
// image: pulling cannot help and its error would hide the real cause.
func TestDockerUnavailableIsNotPulled(t *testing.T) {
	calls := installBrokenDocker(t)
	var frames []object
	_, err := resolveHelperImage(context.Background(), defaultHelperImage, 5, func(v object) error {
		frames = append(frames, v)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "无法通过 Docker 确认") || !strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Fatalf("unreachable daemon was misreported: %v", err)
	}
	if len(frames) != 0 {
		t.Fatalf("a pull was announced against an unreachable daemon: %#v", frames)
	}
	raw, readErr := os.ReadFile(calls)
	if readErr != nil || strings.Contains(string(raw), "pull") {
		t.Fatalf("a pull was attempted against an unreachable daemon: %v %q", readErr, raw)
	}
}

// A Docker-backend scan must not fall back to host scanning when the helper
// cannot be prepared, which hides unreadable paths behind a partial result.
func TestDockerBackendDoesNotFallBackToHost(t *testing.T) {
	installMissingHelperImageDocker(t, true)
	c := defaultConfig()
	c.ScanBackend = "docker"
	if _, err := collectRequest(context.Background(), helperRequest{Version: 1, Config: c, Paths: []string{t.TempDir()}, Mounts: []MountInfo{}}, nil); err == nil {
		t.Fatal("broken helper fell back to a host scan")
	}
}
