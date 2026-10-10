package rootless

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSupervisorStopsOnLeaseEOFAndSignals(t *testing.T) {
	for _, event := range []string{"lease-eof", "signal", "timeout", "child-exit"} {
		t.Run(event, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dir := t.TempDir()
			ready, stopped := dir+"/ready", dir+"/stopped"
			script := "trap 'touch \"$2\"; exit 0' TERM; touch \"$1\"; while :; do sleep 0.05; done"
			if event == "timeout" {
				script = "trap '' TERM; touch \"$1\"; exec sleep 60"
			}
			if event == "child-exit" {
				script = "touch \"$1\"; exit 7"
			}
			cmd := exec.Command("sh", "-c", script, "sh", ready, stopped)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			done := make(chan error, 1)
			go func() { done <- superviseProcess(ctx, reader, cmd, 200*time.Millisecond) }()
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, err = os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("child never started")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if event == "signal" {
				cancel()
			} else if event != "child-exit" {
				writer.Close()
			}
			select {
			case err = <-done:
				if (err != nil) != (event == "timeout" || event == "child-exit") {
					t.Fatal(event, err)
				}
			case <-time.After(3 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				t.Fatal("supervisor failed to stop child")
			}
			if syscall.Kill(cmd.Process.Pid, 0) == nil {
				t.Fatal("child survived")
			}
			if event == "lease-eof" || event == "signal" {
				if _, err = os.Stat(stopped); err != nil {
					t.Fatal("graceful stop not delivered", err)
				}
			}
		})
	}
}
func TestBindingStoreRejectsCorruptionAndRetainsData(t *testing.T) {
	file := t.TempDir() + "/bindings.json"
	if store, err := loadBindings(file); err != nil || store.Version != 1 || store.Bindings == nil || len(store.Bindings) != 0 {
		t.Fatal("missing file must initialize an empty store", store, err)
	}
	for _, text := range []string{`{}`, `null`, `{"version":1}`, `{"bindings":[]}`, `{"version":null,"bindings":[]}`, `{"version":99,"bindings":[]}`, `{"version":1,"bindings":null}`, `{"version":1,"bindings":[{"host":"tcp://remote","container":"c","name":"c","socket_path":"/sock"}]}`, `{"version":1,"bindings":[],"legacy":1}`, `{`} {
		writeTestFile(t, file, text, 0600)
		if _, err := loadBindings(file); err == nil {
			t.Fatal("accepted invalid store", text)
		}
		if got := readTestFile(t, file); got != text {
			t.Fatal("invalid store changed")
		}
	}
	store := bindingStore{Version: 1, Bindings: []binding{{Host: testHost, Container: "id", Name: "name", SocketPath: "/run/docker.sock"}}}
	data, err := jsonBytes(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveBindings(file, data); err != nil {
		t.Fatal(err)
	}
	if got, err := loadBindings(file); err != nil || len(got.Bindings) != 1 {
		t.Fatal(got, err)
	}
	st, _ := os.Stat(file)
	if st.Mode().Perm() != 0600 {
		t.Fatal("state permissions", st.Mode())
	}
	target := file + ".other"
	if err := os.Rename(file, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBindings(file); err == nil {
		t.Fatal("followed state symlink")
	}
}
func TestDetachChecksBootAndContainerIncarnation(t *testing.T) {
	for _, kind := range []string{"same", "reboot", "restarted", "deleted", "docker-error", "foreign-mount"} {
		t.Run(kind, func(t *testing.T) {
			m, u := testManager(t)
			d := &daemon{manager: m, user: u, bootID: "boot"}
			b := binding{Host: testHost, Container: "id", BootID: "boot", PID: 42, StartedAt: "first", SocketPath: "/run/socket", Receipt: &mountReceipt{Namespace: 1, Device: 2, Inode: 3, MountID: "4"}}
			if kind == "reboot" {
				d.bootID = "new"
			}
			m.run = func(args, env []string, _ time.Duration, _ bool) (result, error) {
				if kind == "docker-error" {
					return result{}, errors.New("connection refused")
				}
				if strings.Contains(strings.Join(args, " "), "container ls") {
					if kind == "deleted" {
						return result{}, nil
					}
					return result{Out: "id\n"}, nil
				}
				state := containerState{Pid: 42, Running: true, StartedAt: "first"}
				if kind == "restarted" {
					state.StartedAt = "later"
				}
				data, _ := json.Marshal([]containerInfo{{Id: "id", State: state}})
				return result{Out: string(data)}, nil
			}
			detached := false
			m.worker = func(r workerRequest, _ bool) error {
				detached = true
				if r.Action != "detach" || r.PID != 42 || r.Receipt.MountID != "4" {
					t.Fatal(r)
				}
				if kind == "foreign-mount" {
					return errors.New("changed mount")
				}
				return nil
			}
			err := d.detach(&b)
			if (err != nil) != (kind == "docker-error" || kind == "foreign-mount") {
				t.Fatal(kind, err)
			}
			if detached != (kind == "same" || kind == "foreign-mount") {
				t.Fatal("unexpected detach", kind, detached)
			}
			if err != nil && b.Receipt == nil {
				t.Fatal("lost receipt after failure")
			}
		})
	}
}

func TestRemovePreservesBindingsOnSaveFailure(t *testing.T) {
	for _, index := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			m, u := testManager(t)
			m.run = func([]string, []string, time.Duration, bool) (result, error) {
				t.Fatal("bindings without receipts do not need Docker")
				return result{}, nil
			}
			dir := t.TempDir()
			file := dir + "/bindings.json"
			d := &daemon{manager: m, user: u, stateFile: file, store: bindingStore{Version: 1, Bindings: []binding{}}}
			for i := 0; i < 3; i++ {
				d.store.Bindings = append(d.store.Bindings, binding{Host: testHost, Container: fmt.Sprintf("id%d", i), Name: fmt.Sprintf("name%d", i), SocketPath: "/socket"})
			}
			if err := d.save(); err != nil {
				t.Fatal(err)
			}
			before := readTestFile(t, file)
			o := options{Host: testHost, Container: fmt.Sprintf("name%d", index), SocketPath: "/socket"}
			d.stateFile = dir + "/unavailable/bindings.json"
			if err := d.remove(o); err == nil {
				t.Fatal("expected failed write")
			}
			memory, err := jsonBytes(d.store)
			if err != nil || string(memory) != before || readTestFile(t, file) != before {
				t.Fatal("failed removal changed other bindings or lost retry state", string(memory), err)
			}
			d.stateFile = file
			if err := d.remove(o); err != nil {
				t.Fatal("remove retry failed", err)
			}
			stored, err := loadBindings(file)
			if err != nil || len(stored.Bindings) != 2 {
				t.Fatal(stored, err)
			}
			for _, b := range stored.Bindings {
				if b.Name == o.Container {
					t.Fatal("removed association persisted")
				}
			}
		})
	}
}

func TestControlProtocolAndDockerProxy(t *testing.T) {
	m, u := testManager(t)
	dir, err := os.MkdirTemp("/tmp", "rootless-proxy-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	u.Home = dir
	if err = os.MkdirAll(layout(u).Run, 0700); err != nil {
		t.Fatal(err)
	}
	backend, err := net.Listen("unix", layout(u).Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	upstream := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "rootless:"+r.URL.Path) })}
	go upstream.Serve(backend)
	defer upstream.Close()
	d := &daemon{manager: m, user: u, ctx: context.Background(), store: bindingStore{Version: 1, Bindings: []binding{}}}
	h := d.handler()
	for _, args := range [][]string{{"daemon"}, {"daemon", "--host-namespaces"}, {"docker", "run", "x"}, {"test", "exec"}, {"add", "c", "--host", "tcp://host"}, {"remove", "c", "--socket-path", "/run/../socket"}} {
		b, _ := json.Marshal(controlRequest{Args: args})
		r := httptest.NewRequest("POST", controlPath, bytes.NewReader(b))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal(args, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", controlPath, strings.NewReader(`{"args":["list"]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var response controlResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Output != "[]\n" {
		t.Fatal(w.Code, w.Body.String())
	}
	r = httptest.NewRequest("GET", "/v1.48/info", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "rootless:/v1.48/info" {
		t.Fatal("proxy did not target rootless daemon", w.Code, w.Body.String())
	}
}
func TestClientProtocolOverUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "rootless-client-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "control.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != controlPath || r.Method != "POST" {
			t.Error("wrong route")
		}
		var request controlRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if len(request.Args) != 1 || request.Args[0] != "status" {
			t.Error(request)
		}
		json.NewEncoder(w).Encode(controlResponse{Output: "diagnostic\n", Error: "dockerd stopped"})
	})}
	go server.Serve(listener)
	defer server.Close()
	out, err := clientRequest(options{ControlSocket: sock}, []string{"status"})
	if out != "diagnostic\n" || err == nil || err.Error() != "dockerd stopped" {
		t.Fatal(out, err)
	}
	t.Setenv("DOCKER_CONTEXT", "wrong")
	t.Setenv("DOCKER_HOST", "tcp://wrong")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	for _, entry := range clientDockerEnv() {
		if strings.HasPrefix(entry, "DOCKER_CONTEXT=") || strings.HasPrefix(entry, "DOCKER_HOST=") || strings.HasPrefix(entry, "DOCKER_TLS_VERIFY=") {
			t.Fatal("leaked Docker target", entry)
		}
	}
}
func TestLeasePeerIdentity(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "rootless-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: dir + "/lease", Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		c, err := listener.AcceptUnix()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		cred, err := peerCredentials(c)
		if err == nil && cred.Uid != uint32(os.Getuid()) {
			err = fmt.Errorf("wrong peer: %v", cred)
		}
		done <- err
	}()
	c, err := net.Dial("unix", dir+"/lease")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(c, "")
}

// Mount orchestration tests mock the worker and never connect to the socket.
// A filesystem socket inode is enough to exercise identity/replacement checks.
func socketFile(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mknod(path, syscall.S_IFSOCK|0600, 0); err != nil {
		t.Fatal(err)
	}
}

func TestBindingsRecoverAfterDaemonSocketAndContainerRestart(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "rootless-recover-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	m, u := testManager(t)
	u.Home = dir
	if err = os.MkdirAll(layout(u).Run, 0700); err != nil {
		t.Fatal(err)
	}
	socketFile(t, layout(u).Socket)
	source := layout(u).Socket
	state := containerState{Pid: os.Getpid(), Running: true, StartedAt: "first"}
	m.readUIDMap = func(int) ([]byte, error) { return []byte("0 0 4294967295"), nil }
	m.run = func(args, env []string, _ time.Duration, _ bool) (result, error) {
		var obj any
		command := strings.Join(args, " ")
		switch {
		case args[0] == "runuser":
			obj = daemonInfo{SecurityOptions: []string{"name=rootless"}, DockerRootDir: layout(u).Data}
		case strings.Contains(command, "container ls"):
			return result{Out: "stable-id\n"}, nil
		case strings.Contains(command, "container inspect"):
			obj = []containerInfo{{Id: "stable-id", State: state}}
		case strings.Contains(command, "info --format"):
			obj = daemonInfo{}
		default:
			t.Fatalf("unexpected command: %v", args)
		}
		data, _ := json.Marshal(obj)
		return result{Out: string(data)}, nil
	}
	mounts := map[string]mountReceipt{}
	serial, detaches, attaches, verifies := 0, 0, 0, 0
	var ns syscall.Stat_t
	if err = syscall.Stat(fmt.Sprintf("/proc/%d/ns/mnt", os.Getpid()), &ns); err != nil {
		t.Fatal(err)
	}
	m.worker = func(r workerRequest, _ bool) error {
		switch r.Action {
		case "attach":
			attaches++
			st, err := socketIdentity(r.Source)
			if err != nil {
				return err
			}
			receipt, ok := mounts[r.Destination]
			if !ok {
				serial++
				receipt = mountReceipt{Namespace: ns.Ino, Device: uint64(st.Dev), Inode: st.Ino, MountID: fmt.Sprint(serial)}
				mounts[r.Destination] = receipt
			}
			return json.NewEncoder(m.out).Encode(receipt)
		case "verify-mount":
			verifies++
			receipt, mounted := mounts[r.Destination]
			if mounted && receipt != *r.Receipt {
				return errors.New("foreign mount")
			}
			return json.NewEncoder(m.out).Encode(mounted)
		case "detach":
			if _, mounted := mounts[r.Destination]; !mounted {
				return nil
			}
			if mounts[r.Destination] != *r.Receipt {
				return errors.New("foreign mount")
			}
			delete(mounts, r.Destination)
			detaches++
			return nil
		default:
			return errors.New("unexpected worker")
		}
	}
	d := &daemon{manager: m, user: u, bootID: "boot", stateFile: dir + "/bindings.json", store: bindingStore{Version: 1, Bindings: []binding{}}}
	options := options{Host: testHost, Container: "target-name", SocketPath: "/run/socket"}
	if err = d.add(options); err != nil {
		t.Fatal(err)
	}
	first := d.store.Bindings[0].Receipt.MountID
	before, err := os.Stat(d.stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.add(options); err != nil {
		t.Fatal(err)
	}
	if serial != 1 || first != d.store.Bindings[0].Receipt.MountID {
		t.Fatal("duplicate add stacked a mount")
	}
	after, err := os.Stat(d.stateFile)
	if err != nil || !os.SameFile(before, after) || attaches != 1 || verifies != 1 {
		t.Fatal("duplicate add rewrote state or repeated attachment", attaches, verifies, err)
	}
	// Recover durable state after a management daemon crash without changing
	// an existing mount when both Docker instances remained alive.
	saved, err := loadBindings(d.stateFile)
	if err != nil {
		t.Fatal(err)
	}
	d.store = saved
	if err = d.reconcile("", ""); err != nil {
		t.Fatal(err)
	}
	if serial != 1 {
		t.Fatal("daemon restart stacked a mount")
	}
	// A new rootless socket must detach the exact old receipt first.
	if err = os.Rename(source, source+".old"); err != nil {
		t.Fatal(err)
	}
	socketFile(t, source)
	if err = d.reconcile("", ""); err != nil {
		t.Fatal(err)
	}
	if serial != 2 || detaches != 1 {
		t.Fatal("socket replacement did not detach old mount", serial, detaches)
	}
	// A restarted container has a new namespace incarnation. Never use its
	// old receipt, even if Linux reuses the same numeric PID.
	state.StartedAt = "second"
	mounts = map[string]mountReceipt{}
	if err = d.reconcile(testHost, "stable-id"); err != nil {
		t.Fatal(err)
	}
	if serial != 3 || detaches != 1 {
		t.Fatal("container restart reused old receipt", serial, detaches)
	}
	// Simulate a crash after unmounting but before clearing the disk receipt.
	// Recovery must work whether dockerd kept or replaced its source socket.
	for _, replaceSource := range []bool{false, true} {
		delete(mounts, options.SocketPath)
		if replaceSource {
			if err = os.Rename(source, source+".second"); err != nil {
				t.Fatal(err)
			}
			socketFile(t, source)
		}
		d.store, err = loadBindings(d.stateFile)
		if err != nil {
			t.Fatal(err)
		}
		if err = d.reconcile("", ""); err != nil {
			t.Fatal("cannot recover after completed detach", replaceSource, err)
		}
		if _, ok := mounts[options.SocketPath]; !ok {
			t.Fatal("recovery did not restore mount", replaceSource)
		}
	}
	// A failed final write must retain the association, without repeating the
	// already completed unmount when the caller retries.
	file := d.stateFile
	d.stateFile = dir + "/missing/bindings.json"
	if err = d.remove(options); err == nil {
		t.Fatal("expected remove persistence failure")
	}
	if len(d.store.Bindings) != 1 || d.store.Bindings[0].Receipt != nil || len(mounts) != 0 {
		t.Fatal("failed remove lost its retry state", d.store.Bindings, mounts)
	}
	d.stateFile = file
	beforeDetaches := detaches
	if err = d.remove(options); err != nil {
		t.Fatal(err)
	}
	if len(d.store.Bindings) != 0 || len(mounts) != 0 || detaches != beforeDetaches {
		t.Fatal("remove did not persist deletion")
	}
	if saved, err = loadBindings(file); err != nil || len(saved.Bindings) != 0 {
		t.Fatal("removed association remained on disk", saved, err)
	}
	if err = d.remove(options); err == nil {
		t.Fatal("removed unowned mount")
	}
}

// Run only in a disposable container sharing the PID namespace of another
// disposable container. The marker belongs to that fixture, never the host.
func TestEnterHostProcess(t *testing.T) {
	if os.Getenv("ROOTLESS_ENTRY_INTEGRATION") != "1" {
		t.Skip("requires isolated container namespace fixture")
	}
	if _, err := os.Stat("/rootless-entry-marker"); err == nil {
		if got := readTestFile(t, "/proc/self/cgroup"); got != os.Getenv("ROOTLESS_ENTRY_CGROUP") {
			t.Fatal("namespace entry moved cgroup")
		}
		return
	}
	os.Setenv("ROOTLESS_ENTRY_CGROUP", readTestFile(t, "/proc/self/cgroup"))
	if err := enterHost([]string{"-test.run=^TestEnterHostProcess$", "-test.v", "--host-namespaces"}); err != nil {
		t.Fatal(err)
	}
}

// Requires root only inside a disposable container, no host namespaces,
// mounts, systemd services or accounts. Executes the actual supervisor binary
// as UID 65534, validates its root peer and closes the owner's socket.
func TestRealUserSupervisorLease(t *testing.T) {
	if os.Getenv("ROOTLESS_LEASE_INTEGRATION") != "1" {
		t.Skip("requires disposable root container")
	}
	if os.Getuid() != 0 {
		t.Fatal("fixture needs container root")
	}
	dir, err := os.MkdirTemp("/tmp", "rootless-lease-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err = os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	work := dir + "/work"
	if err = os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Chown(work, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	ready, stopped := work+"/ready", work+"/stopped"
	launch := dir + "/launch.sh"
	writeTestFile(t, launch, "#!/bin/sh\ntrap 'touch "+stopped+"; exit 0' TERM\ntouch "+ready+"\nwhile :; do sleep 0.1; done\n", 0755)
	lease, err := startLeaseServer(dir+"/lease.sock", 65534)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	cmd := exec.Command(os.Args[0], "--internal-supervisor", dir+"/lease.sock", launch)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill() }()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err = os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			<-done
			t.Fatal("supervisor not ready", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	lease.Close()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err, output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not stop on EOF")
	}
	if _, err = os.Stat(stopped); err != nil {
		t.Fatal("dockerd substitute did not receive graceful stop", err)
	}
}

func TestBindingStatusesVerifyLiveIdentity(t *testing.T) {
	m, u := testManager(t)
	u.Home = t.TempDir()
	if err := os.MkdirAll(layout(u).Run, 0700); err != nil {
		t.Fatal(err)
	}
	socketFile(t, layout(u).Socket)
	source, err := socketIdentity(layout(u).Socket)
	if err != nil {
		t.Fatal(err)
	}
	state := containerState{Pid: os.Getpid(), Running: true, StartedAt: "first"}
	unavailable, missing, mounted := false, false, true
	m.run = func(args, env []string, _ time.Duration, _ bool) (result, error) {
		if unavailable {
			return result{}, errors.New("Docker unavailable")
		}
		if strings.Contains(strings.Join(args, " "), "container ls") {
			if missing {
				return result{}, nil
			}
			return result{Out: "stable-id"}, nil
		}
		raw, _ := json.Marshal([]containerInfo{{Id: "stable-id", State: state}})
		return result{Out: string(raw)}, nil
	}
	m.worker = func(r workerRequest, _ bool) error {
		if r.Action != "verify-mount" {
			t.Fatal("observation changed mount", r.Action)
		}
		return json.NewEncoder(m.out).Encode(mounted)
	}
	d := &daemon{manager: m, user: u, bootID: "boot", store: bindingStore{Version: 1, Bindings: []binding{{Host: testHost, Container: "stable-id", Name: "name", SocketPath: "/sock", BootID: "boot", PID: state.Pid, StartedAt: state.StartedAt, Receipt: &mountReceipt{Namespace: 1, Device: uint64(source.Dev), Inode: source.Ino, MountID: "4"}}}}}
	check := func(want string) {
		t.Helper()
		before, _ := json.Marshal(d.store)
		got := d.bindingStatuses()
		after, _ := json.Marshal(d.store)
		if len(got) != 1 || got[0]["state"] != want {
			t.Fatal(got, want)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("observation changed persistence")
		}
	}
	check("mounted")
	mounted = false
	check("pending")
	mounted = true
	state.StartedAt = "restarted"
	check("pending")
	state.StartedAt = "first"
	d.bootID = "new-boot"
	check("pending")
	d.bootID = "boot"
	state.Running = false
	check("waiting")
	state.Running = true
	missing = true
	check("missing")
	missing = false
	unavailable = true
	check("unknown")
	unavailable = false
	if err := os.Rename(layout(u).Socket, layout(u).Socket+".old"); err != nil {
		t.Fatal(err)
	}
	socketFile(t, layout(u).Socket)
	check("pending")
}
