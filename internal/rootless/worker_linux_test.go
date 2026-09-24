package rootless

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--internal-worker" {
		os.Exit(Main(os.Args[1:]))
	}
	os.Exit(m.Run())
}
func TestConfigurationWorkerReexec(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("configuration workers must run as a non-root account")
	}
	_, u := testManager(t)
	// Execute the real re-exec protocol with the current unprivileged account.
	// Production starts this same worker with docker-rootless credentials.
	if err := runWorker(workerRequest{Action: "prepare", User: u}, false); err != nil {
		t.Fatal(err)
	}
	cfg := readConfig(t, u)
	if cfg["data-root"] != layout(u).Data {
		t.Fatal(cfg)
	}
	if err := runWorker(workerRequest{Action: "show-proxy", User: u}, false); err != nil {
		t.Fatal(err)
	}
	bad := u
	bad.UID++
	if err := runWorker(workerRequest{Action: "prepare", User: bad}, false); err == nil {
		t.Fatal("worker accepted mismatched credentials")
	}
	if err := runWorker(workerRequest{Action: "unknown", User: u}, false); err == nil {
		t.Fatal("worker accepted unknown operation")
	}
}

// Run with ROOTLESS_WORKER_INTEGRATION=1 rootlesskit <test-binary>
// -test.run '^TestWorkerPrivilegeIntegration$'. Multiple subordinate IDs are
// required; no host account, service or Docker daemon is touched.
func TestWorkerPrivilegeIntegration(t *testing.T) {
	if os.Getenv("ROOTLESS_WORKER_INTEGRATION") != "1" {
		t.Skip("requires rootlesskit with subordinate UID/GID mappings")
	}
	mapping := strings.Join(strings.Fields(readTestFile(t, "/proc/self/uid_map")), " ")
	if os.Geteuid() != 0 || mapping == "0 0 4294967295" {
		t.Fatal("must run inside a rootless user namespace, not as host root")
	}
	for _, name := range []string{"dockerd", "dockerd-rootless.sh"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatal(err)
		}
	}
	base, err := os.MkdirTemp("", "rootless-worker-privileges-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(base, 0711); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(base, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(private, "rootless-worker.test")
	source, err := os.Open("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.OpenFile(binary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil {
		t.Fatal(copyErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	home := filepath.Join(base, "home")
	if err = os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Chown(home, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(base, "secret")
	writeTestFile(t, secret, "root only", 0600)
	for _, test := range []string{"TestWorkerPrivateExecutableHelper", "TestWorkerCredentialThreadsHelper"} {
		cmd := exec.Command(binary, "-test.run=^"+test+"$", "-test.v")
		cmd.Env = append(os.Environ(), "ROOTLESS_WORKER_HOME="+home, "ROOTLESS_WORKER_SECRET="+secret)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", test, err, output)
		} else {
			t.Logf("%s", output)
		}
	}
	if os.Geteuid() != 0 || os.Getegid() != 0 {
		t.Fatal("worker changed manager credentials")
	}
}

func TestWorkerPrivateExecutableHelper(t *testing.T) {
	home := os.Getenv("ROOTLESS_WORKER_HOME")
	if home == "" {
		return
	}
	u := account{Home: home, UID: 1000, GID: 1000, Groups: []uint32{1000, 1001}}
	// This is the same root-owned 0700 inode used by runWorker's re-exec. The
	// target user must not need read/execute access to it or its parent directory.
	executable, err := os.Open("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	defer executable.Close()
	// Reproduce the previous launch order, including the inherited executable FD.
	restricted := exec.Command("/proc/self/fd/3", "--help")
	restricted.ExtraFiles = []*os.File{executable}
	restricted.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: u.UID, Gid: u.GID, Groups: u.Groups}}
	if err = restricted.Run(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected private executable to reject target user: %v", err)
	}
	allow := true
	config := options{AllowLoopback: &allow, Proxies: map[string]string{
		"http-proxy": "http://10.0.2.2:13099", "https-proxy": "http://10.0.2.2:13099", "no-proxy": "localhost,127.0.0.1",
	}}
	if err = runWorker(workerRequest{Action: "prepare", User: u, Options: config}, true); err != nil {
		t.Fatal(err)
	}
	if enabled, err := networkSettings(u); err != nil || !enabled {
		t.Fatal("loopback setting not applied", enabled, err)
	}
	proxies := readConfig(t, u)["proxies"].(map[string]any)
	for key, value := range config.Proxies {
		if proxies[key] != value {
			t.Fatalf("proxy %s: got %v", key, proxies[key])
		}
	}
	for _, file := range []string{layout(u).Config + "/daemon.json", layout(u).Config + "/rootlesskit.json", layout(u).Base + "/launch.sh", layout(u).Unit} {
		st, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		owner := st.Sys().(*syscall.Stat_t)
		if owner.Uid != u.UID || owner.Gid != u.GID {
			t.Fatalf("configuration written with wrong credentials: %s uid=%d gid=%d", file, owner.Uid, owner.Gid)
		}
	}
	if err = runWorker(workerRequest{Action: "show-proxy", User: u}, true); err != nil {
		t.Fatal(err)
	}
	// Refuse an invalid target before accessing configuration, including reads.
	invalid := u
	invalid.UID = 0
	if err = runWorker(workerRequest{Action: "show-proxy", User: invalid}, true); err == nil {
		t.Fatal("accepted root target")
	}
	invalid = u
	invalid.Groups = []uint32{0}
	if err = runWorker(workerRequest{Action: "show-proxy", User: invalid}, true); err == nil {
		t.Fatal("kept root supplementary group")
	}
	if os.Geteuid() != 0 || os.Getegid() != 0 {
		t.Fatal("manager lost privileges")
	}
}

func TestWorkerCredentialThreadsHelper(t *testing.T) {
	home := os.Getenv("ROOTLESS_WORKER_HOME")
	if home == "" {
		return
	}
	u := account{Home: home, UID: 1000, GID: 1000, Groups: []uint32{1000, 1001}}
	const threads = 8
	ready := make(chan struct{}, threads)
	check := make(chan struct{})
	results := make(chan error, threads)
	// Hold several pre-existing OS threads so this detects thread-local setuid
	// mistakes in both libc-linked and CGO_ENABLED=0 builds.
	for range threads {
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			ready <- struct{}{}
			<-check
			results <- checkDroppedCredentials(u, os.Getenv("ROOTLESS_WORKER_SECRET"))
		}()
	}
	for range threads {
		<-ready
	}
	if err := dropWorkerPrivileges(u); err != nil {
		t.Fatal(err)
	}
	close(check)
	for range threads {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if err := checkDroppedCredentials(u, os.Getenv("ROOTLESS_WORKER_SECRET")); err != nil {
		t.Error(err)
	}
	cwd, err := os.Getwd()
	if err != nil || cwd != home {
		t.Fatal("worker did not enter target home", cwd, err)
	}
}

func checkDroppedCredentials(u account, secret string) error {
	r, e, s := unix.Getresuid()
	if r != int(u.UID) || e != int(u.UID) || s != int(u.UID) {
		return fmt.Errorf("UIDs not fully dropped: %d/%d/%d", r, e, s)
	}
	r, e, s = unix.Getresgid()
	if r != int(u.GID) || e != int(u.GID) || s != int(u.GID) {
		return fmt.Errorf("GIDs not fully dropped: %d/%d/%d", r, e, s)
	}
	groups, err := syscall.Getgroups()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(groups, []int{1000, 1001}) {
		return fmt.Errorf("unexpected supplementary groups: %v", groups)
	}
	if _, err = os.ReadFile(secret); !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("root-owned secret was accessible: %v", err)
	}
	// A raw thread-local syscall is intentional here: a saved root UID must not
	// remain on any thread. Production uses process-wide setters to drop it.
	if err = unix.Setresuid(0, 0, 0); !errors.Is(err, unix.EPERM) {
		return fmt.Errorf("root credentials could be restored: %v", err)
	}
	return nil
}
