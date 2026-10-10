package rootless

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestConfinedContainerPaths(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"var", "run"} {
		if err := os.Mkdir(root+"/"+dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/run", root+"/var/run"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root+"/run/docker.sock", "", 0600)
	fd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	target, err := confinedOpen(fd, "/var/run/docker.sock", unix.O_PATH, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(target)
	var actual, want unix.Stat_t
	unix.Fstat(target, &actual)
	unix.Stat(root+"/run/docker.sock", &want)
	if actual.Ino != want.Ino {
		t.Fatal("absolute symlink escaped container root")
	}
	if err = ensureParent(fd, "/var/run/custom/nested/docker.sock", nil); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(root + "/run/custom/nested"); err != nil || !st.IsDir() {
		t.Fatal(st, err)
	}
	host, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(host)
	escaped, err := confinedOpen(host, "/proc/self/root/etc/passwd", unix.O_PATH, 0)
	if err == nil {
		unix.Close(escaped)
		t.Fatal("accepted proc magic link")
	}
	if !errors.Is(err, unix.ELOOP) {
		t.Fatal(err)
	}
	called := false
	err = ensureParent(fd, "/forbidden/path/socket", func(int) error { called = true; return errors.New("shared mount") })
	if err == nil || !called {
		t.Fatal("creation guard not called")
	}
	if _, err = os.Stat(root + "/forbidden"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created on rejected mount")
	}
}
func TestMountPropagationGuard(t *testing.T) {
	for _, tt := range []struct {
		text string
		bad  bool
	}{{"24 1 0:1 / / rw - ext4 /dev/root rw", false}, {"24 1 0:1 / / rw shared:1 master:2 - ext4 /dev/root rw", true}, {"25 1 0:1 / / rw - ext4 /dev/root rw", true}, {"24 1 0:1 / / rw master:2 - ext4 /dev/root rw", false}, {"24 1 0:1 / / rw invalid", true}} {
		if err := checkMountInfo("24", tt.text); (err != nil) != tt.bad {
			t.Fatal(tt, err)
		}
	}
}

// This test process is also the disposable setns worker and the isolated target.
// No account, Docker service or host mount is modified by the integration test.
func TestMountHelperProcess(t *testing.T) {
	mode := os.Getenv("ROOTLESS_TEST_HELPER")
	if mode == "" {
		return
	}
	var err error
	switch mode {
	case "attach":
		err = workerMain()
	case "target":
		err = mountTarget()
	default:
		err = errors.New("unknown helper")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
func mountTarget() error {
	runtime.LockOSThread()
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return err
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return err
	}
	root := os.Getenv("ROOTLESS_TEST_ROOT")
	if err := unix.Mount(root, root, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	if os.Getenv("ROOTLESS_TEST_SHARED") == "1" {
		if err := unix.Mount(root+"/run", root+"/run", "", unix.MS_BIND, ""); err != nil {
			return err
		}
		if err := unix.Mount("", root+"/run", "", unix.MS_SHARED, ""); err != nil {
			return err
		}
	}
	if err := unix.Chroot(root); err != nil {
		return err
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}
	if _, err := os.Stdout.Write([]byte("R")); err != nil {
		return err
	}
	var b [1]byte
	for {
		if _, err := io.ReadFull(os.Stdin, b[:]); err != nil {
			return nil
		}
		if b[0] != 'P' {
			return errors.New("invalid probe")
		}
		fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			return err
		}
		if err = unix.Connect(fd, &unix.SockaddrUnix{Name: "/var/run/docker.sock"}); err != nil {
			unix.Close(fd)
			return err
		}
		if _, err = unix.Write(fd, []byte("ping")); err != nil {
			unix.Close(fd)
			return err
		}
		var reply [4]byte
		n, err := unix.Read(fd, reply[:])
		unix.Close(fd)
		if err != nil {
			return err
		}
		if n != 4 || string(reply[:]) != "pong" {
			return errors.New("bad response")
		}
		if _, err = os.Stdout.Write([]byte("O")); err != nil {
			return err
		}
	}
}
func TestRealSocketMounts(t *testing.T) {
	if os.Getenv("ROOTLESS_MOUNT_INTEGRATION") != "1" {
		t.Skip("run explicitly inside unshare; see docs/rootless-docker.md")
	}
	mapping := strings.Join(strings.Fields(readTestFile(t, "/proc/self/uid_map")), " ")
	if os.Geteuid() != 0 || mapping == "0 0 4294967295" {
		t.Fatal("must run in an isolated user/mount namespace")
	}
	for _, kind := range []string{"existing", "missing", "shared", "shared-source", "nonempty", "wrong-owner", "source-symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if kind == "shared-source" {
				if err := unix.Mount(dir, dir, "", unix.MS_BIND, ""); err != nil {
					t.Fatal(err)
				}
				defer unix.Unmount(dir, unix.MNT_DETACH)
				if err := unix.Mount("", dir, "", unix.MS_SHARED, ""); err != nil {
					t.Fatal(err)
				}
			}
			root := dir + "/rootfs"
			for _, sub := range []string{"run", "var"} {
				if err := os.MkdirAll(root+"/"+sub, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("/run", root+"/var/run"); err != nil {
				t.Fatal(err)
			}
			source := dir + "/source.sock"
			listener := socketListener(t, source)
			defer func() { listener.Close() }()
			var original *net.UnixListener
			if kind == "existing" {
				original = socketListener(t, root+"/run/docker.sock")
				defer original.Close()
			}
			if kind == "nonempty" {
				writeTestFile(t, root+"/run/docker.sock", "preserve", 0600)
			}
			target := exec.Command(os.Args[0], "-test.run=^TestMountHelperProcess$")
			target.Env = append(os.Environ(), "ROOTLESS_TEST_HELPER=target", "ROOTLESS_TEST_ROOT="+root)
			if kind == "shared" {
				target.Env = append(target.Env, "ROOTLESS_TEST_SHARED=1")
			}
			target.SysProcAttr = &unix.SysProcAttr{Cloneflags: unix.CLONE_NEWNS}
			input, err := target.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := target.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			target.Stderr = &stderr
			if err = target.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { input.Close(); target.Process.Kill(); target.Wait() }()
			// Pipe deadlines make failures bounded even if the helper cannot become ready.
			output.(*os.File).SetReadDeadline(time.Now().Add(10 * time.Second))
			var ready [1]byte
			if _, err = io.ReadFull(output, ready[:]); err != nil || ready[0] != 'R' {
				target.Wait()
				t.Fatalf("target ready: %v %s", err, stderr.String())
			}
			probe := func(l *net.UnixListener) {
				t.Helper()
				if _, err = input.Write([]byte("P")); err != nil {
					t.Fatal(err)
				}
				l.SetDeadline(time.Now().Add(5 * time.Second))
				c, err := l.Accept()
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				var ping [4]byte
				if _, err = io.ReadFull(c, ping[:]); err != nil || string(ping[:]) != "ping" {
					t.Fatal(err, string(ping[:]))
				}
				if _, err = c.Write([]byte("pong")); err != nil {
					t.Fatal(err)
				}
				output.(*os.File).SetReadDeadline(time.Now().Add(5 * time.Second))
				if _, err = io.ReadFull(output, ready[:]); err != nil || ready[0] != 'O' {
					t.Fatal(err, string(ready[:]))
				}
			}
			count := func() int {
				return len(strings.Split(strings.TrimSpace(readTestFile(t, fmt.Sprintf("/proc/%d/mountinfo", target.Process.Pid))), "\n"))
			}
			owner := uint32(os.Getuid())
			if kind == "wrong-owner" {
				owner++
			}
			if kind == "source-symlink" {
				link := dir + "/link.sock"
				if err = os.Symlink(source, link); err != nil {
					t.Fatal(err)
				}
				source = link
			}
			var receipt mountReceipt
			callWorker := func(r workerRequest, response any) error {
				r.PID, r.Destination = target.Process.Pid, "/var/run/docker.sock"
				data, _ := json.Marshal(r)
				cmd := exec.Command(os.Args[0], "-test.run=^TestMountHelperProcess$")
				cmd.Env = append(os.Environ(), "ROOTLESS_TEST_HELPER=attach")
				cmd.Stdin = bytes.NewReader(data)
				out, e := cmd.CombinedOutput()
				if e != nil {
					return fmt.Errorf("%w: %s", e, out)
				}
				if response != nil {
					return json.Unmarshal(out, response)
				}
				return nil
			}
			attach := func() error {
				return callWorker(workerRequest{Action: "attach", User: account{UID: owner}, Source: source}, &receipt)
			}
			detach := func(r mountReceipt) error {
				return callWorker(workerRequest{Action: "detach", Receipt: &r}, nil)
			}
			verify := func(r mountReceipt) (bool, error) {
				var mounted bool
				err := callWorker(workerRequest{Action: "verify-mount", Receipt: &r}, &mounted)
				return mounted, err
			}
			before := count()
			if kind == "shared" || kind == "nonempty" || kind == "wrong-owner" || kind == "source-symlink" {
				if err = attach(); err == nil {
					t.Fatal("unsafe mount accepted")
				}
				if count() != before {
					t.Fatal("rejected mount changed namespace")
				}
				if kind == "nonempty" && readTestFile(t, root+"/run/docker.sock") != "preserve" {
					t.Fatal("overwrote target")
				}
				return
			}
			var old unix.Stat_t
			if original != nil {
				probe(original)
				if err = unix.Stat(root+"/run/docker.sock", &old); err != nil {
					t.Fatal(err)
				}
			}
			if err = attach(); err != nil {
				t.Fatal(err)
			}
			if count() != before+1 {
				t.Fatal("expected one new mount")
			}
			probe(listener)
			if err = checkMountInfo(receipt.MountID, readTestFile(t, fmt.Sprintf("/proc/%d/mountinfo", target.Process.Pid))); err != nil {
				t.Fatal("injected mount inherited shared propagation", err)
			}
			if err = attach(); err != nil {
				t.Fatal(err)
			}
			if count() != before+1 {
				t.Fatal("duplicate mount stacked")
			}
			if original != nil {
				var current unix.Stat_t
				unix.Stat(root+"/run/docker.sock", &current)
				if current.Ino != old.Ino {
					t.Fatal("underlying socket replaced")
				}
			}
			foreign := receipt
			// A different mount still present in this namespace cannot authorize
			// unmounting the socket visible at the destination.
			foreign.MountID = strings.Fields(readTestFile(t, fmt.Sprintf("/proc/%d/mountinfo", target.Process.Pid)))[0]
			if err = detach(foreign); err == nil {
				t.Fatal("unmounted an unowned mount")
			}
			if count() != before+1 {
				t.Fatal("rejected detach changed mounts")
			}
			if mounted, err := verify(receipt); err != nil || !mounted {
				t.Fatal("live mount not verified", mounted, err)
			}
			// A mount covering the recorded one must remain untouched. The old
			// mount is still in mountinfo even though it is not at the path's top.
			replacementSource := dir + "/replacement.sock"
			replacement := socketListener(t, replacementSource)
			defer replacement.Close()
			var replacementReceipt mountReceipt
			mountReplacement := func() error {
				return callWorker(workerRequest{Action: "attach", User: account{UID: owner}, Source: replacementSource}, &replacementReceipt)
			}
			if err = mountReplacement(); err != nil {
				t.Fatal(err)
			}
			if err = detach(receipt); err == nil {
				t.Fatal("detached a replacement covering the recorded mount")
			}
			if _, err = verify(receipt); err == nil || count() != before+2 {
				t.Fatal("covered mount was mistaken for a completed detach", err)
			}
			probe(replacement)
			if err = detach(replacementReceipt); err != nil {
				t.Fatal(err)
			}
			if err = detach(receipt); err != nil {
				t.Fatal(err)
			}
			if count() != before {
				t.Fatal("detach did not restore mount count")
			}
			if err = detach(receipt); err != nil {
				t.Fatal("cannot replay detach after a crash before saving", err)
			}
			if count() != before {
				t.Fatal("replayed detach changed the underlying mount")
			}
			if mounted, err := verify(receipt); err != nil || mounted {
				t.Fatal("completed detach still reported as mounted", mounted, err)
			}
			// Even if another mount appears after removal, replaying the old
			// receipt must leave that replacement and its socket usable.
			if err = mountReplacement(); err != nil {
				t.Fatal(err)
			}
			err = detach(receipt)
			if count() != before+1 {
				t.Fatal("replayed detach touched a later replacement", err)
			}
			// Linux may reuse the mount ID immediately. In that case the inode
			// mismatch must remain an error; it must never authorize removal.
			if (err != nil) != (receipt.MountID == replacementReceipt.MountID) {
				t.Fatal("unexpected replacement identity result", receipt, replacementReceipt, err)
			}
			probe(replacement)
			if err = detach(replacementReceipt); err != nil {
				t.Fatal(err)
			}
			if original != nil {
				probe(original)
			}
			if err = attach(); err != nil {
				t.Fatal(err)
			}
			listener.Close()
			if err = detach(receipt); err != nil {
				t.Fatal("cannot detach unlinked source", err)
			}
			listener = socketListener(t, source)
			if err = attach(); err != nil {
				t.Fatal(err)
			}
			if count() != before+1 {
				t.Fatal("stale mount not detached")
			}
			probe(listener)
		})
	}
}
func socketListener(t *testing.T, path string) *net.UnixListener {
	t.Helper()
	if len(path) >= 108 {
		t.Fatal("socket test path too long", filepath.Base(path))
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	return l
}
