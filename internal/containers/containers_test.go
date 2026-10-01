package containers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

type fakeDocker struct {
	c          inspection
	daemonID   string
	calls      [][]string
	inputs     []string
	failAction string
	absent     bool
	sshPort    int
}

func (f *fakeDocker) run(_ context.Context, _ string, args []string, input string) (string, error) {
	f.calls = append(f.calls, slices.Clone(args))
	f.inputs = append(f.inputs, input)
	if args[0] == f.failAction {
		return "", fmt.Errorf("模拟 %s 失败", args[0])
	}
	switch args[0] {
	case "info":
		return `{"ID":"` + f.daemonID + `","OSType":"linux"}`, nil
	case "container":
		if f.absent {
			return "", fmt.Errorf("No such container")
		}
		raw, _ := json.Marshal([]inspection{f.c})
		return string(raw), nil
	case "ps":
		if f.absent {
			return "", nil
		}
		return f.c.ID + "\n", nil
	case "image":
		return "[]", nil
	case "create":
		f.absent = false
		f.c.Mounts = nil
		f.c.HostConfig.PortBindings = nil
		f.c.Config.Cmd = args[len(args)-2:]
		for i, a := range args {
			if a == "--label" {
				if f.c.Config.Labels == nil {
					f.c.Config.Labels = map[string]string{}
				}
				key, value, _ := strings.Cut(args[i+1], "=")
				f.c.Config.Labels[key] = value
			}
			if a == "--network" {
				f.c.HostConfig.NetworkMode = args[i+1]
			}
			if a == "--user" {
				f.c.Config.User = args[i+1]
			}
			if a == "--mount" {
				fields := strings.Split(args[i+1], ",")
				f.c.Mounts = append(f.c.Mounts, mount{"bind", strings.TrimPrefix(fields[1], "src="), strings.TrimPrefix(fields[2], "dst="), true})
			}
			if a == "--name" {
				f.c.Name = "/" + args[i+1]
			}
			if a == "--hostname" {
				f.c.Config.Hostname = args[i+1]
			}
			if a == "--publish" {
				f.c.HostConfig.PortBindings = map[string][]binding{"22/tcp": {{HostPort: strings.Split(args[i+1], ":")[0]}}}
			}
		}
		f.c.State.Status = "created"
		f.c.State.Running = false
		return f.c.ID + "\n", nil
	case "exec":
		if match := regexp.MustCompile(`1iPort ([0-9]+)`).FindStringSubmatch(strings.Join(args, " ")); match != nil {
			f.sshPort, _ = strconv.Atoi(match[1])
		}
		if slices.Contains(args, "-T") {
			return fmt.Sprintf("port %d\npasswordauthentication yes\n", f.sshPort), nil
		}
		return "", nil
	case "start", "restart":
		f.c.State.Status = "running"
		f.c.State.Running = true
		return f.c.ID, nil
	case "stop":
		f.c.State.Status = "exited"
		f.c.State.Running = false
		return f.c.ID, nil
	case "rm":
		f.absent = true
		return f.c.ID, nil
	}
	return "", fmt.Errorf("unexpected command: %v", args)
}
func fixture(t *testing.T) (*Handler, *fakeDocker, Config) {
	t.Helper()
	db, err := platform.OpenDatabase(t.TempDir(), Initialize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	cfg := defaults()
	cfg.BaseDir = t.TempDir()
	cfg.Image = "training:test"
	for _, path := range []string{"alice/home", "alice/workspace", "data"} {
		if err = os.MkdirAll(filepath.Join(cfg.BaseDir, path), 0755); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(cfg)
	if _, err = db.SQL.Exec("UPDATE container_settings SET value=?", string(raw)); err != nil {
		t.Fatal(err)
	}
	f := &fakeDocker{daemonID: "daemon-a", sshPort: 22}
	c := &f.c
	c.ID = strings.Repeat("a", 64)
	c.Name = "/alice"
	c.Image = "sha256:abc"
	c.Config.Image = cfg.Image
	c.Config.Hostname = "docker-alice"
	c.Config.Tty = true
	c.Config.Cmd = []string{"/bin/bash", "-c", "service ssh restart && echo 'root:legacy-secret' | chpasswd && /bin/bash"}
	c.HostConfig.NetworkMode = "bridge"
	c.HostConfig.IpcMode = "host"
	c.HostConfig.RestartPolicy.Name = "unless-stopped"
	c.HostConfig.Ulimits = append(c.HostConfig.Ulimits, struct {
		Name       string
		Soft, Hard int64
	}{"memlock", -1, -1})
	c.HostConfig.DeviceRequests = []device{{Driver: "nvidia", Count: -1, Capabilities: [][]string{{"gpu"}}}}
	c.HostConfig.PortBindings = map[string][]binding{"22/tcp": {{HostPort: "2222"}}}
	c.State.Running = true
	c.State.Status = "running"
	for _, dest := range []string{"home", "workspace", "data"} {
		source := filepath.Join(cfg.BaseDir, "alice", dest)
		if dest == "data" {
			source = filepath.Join(cfg.BaseDir, "data")
		}
		c.Mounts = append(c.Mounts, mount{"bind", source, "/" + dest, true})
	}
	h := NewHandler(db)
	h.run = f.run
	return h, f, cfg
}

var admin = platform.User{ID: "admin-id", Username: "admin", Role: "admin"}

func call(h *Handler, method, path, body string, user platform.User) (int, string) {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	status, value, err := h.Dispatch(httptest.NewRecorder(), r, user)
	if err != nil {
		return status, err.Error()
	}
	raw, _ := json.Marshal(value)
	return status, string(raw)
}
func adopt(t *testing.T, h *Handler) {
	t.Helper()
	if err := h.Import(context.Background(), ImportOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
}
func TestAdoptAndLifecycle(t *testing.T) {
	h, f, _ := fixture(t)
	adopt(t, h)
	rows, err := h.records()
	if err != nil || len(rows) != 1 || rows[0].Spec.Port != 2222 || rows[0].Spec.GPUs != "all" {
		t.Fatalf("records: %+v %v", rows, err)
	}
	for _, args := range f.calls {
		if !slices.Contains([]string{"info", "container", "exec", "ps"}, args[0]) {
			t.Fatalf("adoption mutated Docker: %v", args)
		}
	}
	path := "/api/containers/" + f.c.ID
	_, body := call(h, "POST", path+"/delete", `{"confirm":"alice"}`, admin)
	if !strings.Contains(body, "仍在运行") {
		t.Fatal(body)
	}
	for _, action := range []string{"stop", "start", "restart", "stop"} {
		status, body := call(h, "POST", path+"/"+action, `{}`, admin)
		if status != 200 {
			t.Fatalf("%s: %s", action, body)
		}
	}
	_, body = call(h, "POST", path+"/delete", `{"confirm":"wrong"}`, admin)
	if !strings.Contains(body, "完整容器名") {
		t.Fatal(body)
	}
	status, body := call(h, "POST", path+"/delete", `{"confirm":"alice"}`, admin)
	if status != 200 {
		t.Fatal(body)
	}
	rows, _ = h.records()
	if len(rows) != 0 {
		t.Fatal("record survived delete")
	}
	if got := f.calls[len(f.calls)-1]; !slices.Equal(got, []string{"rm", f.c.ID}) {
		t.Fatalf("unsafe delete: %v", got)
	}
	for _, m := range f.c.Mounts {
		if _, err = os.Stat(m.Source); err != nil {
			t.Fatalf("bind data deleted: %v", err)
		}
	}
}
func TestAdoptionFailureReasons(t *testing.T) {
	cases := []struct {
		name, want string
		change     func(*fakeDocker, Config)
	}{
		{"stopped", "运行状态", func(f *fakeDocker, _ Config) { f.c.State.Running = false; f.c.State.Status = "exited" }},
		{"paused", "运行状态", func(f *fakeDocker, _ Config) { f.c.State.Paused = true }},
		{"mount", "挂载 /home", func(f *fakeDocker, _ Config) { f.c.Mounts[0].Source = "/wrong" }},
		{"readonly", "挂载 /home", func(f *fakeDocker, _ Config) { f.c.Mounts[0].RW = false }},
		{"extra mount", "挂载数量", func(f *fakeDocker, _ Config) { f.c.Mounts = append(f.c.Mounts, mount{Destination: "/etc"}) }},
		{"missing directory", "宿主机目录 /home", func(_ *fakeDocker, cfg Config) { os.Remove(filepath.Join(cfg.BaseDir, "alice/home")) }},
		{"network", "网络", func(f *fakeDocker, _ Config) { f.c.HostConfig.NetworkMode = "custom" }},
		{"ports", "SSH 端口映射", func(f *fakeDocker, _ Config) { f.c.HostConfig.PortBindings = nil }},
		{"gpu", "GPU", func(f *fakeDocker, _ Config) { f.c.HostConfig.DeviceRequests = nil }},
		{"ssh", "SSH 有效配置", func(f *fakeDocker, _ Config) { f.failAction = "exec" }},
		{"permission", "Docker 连接", func(f *fakeDocker, _ Config) { f.failAction = "info" }},
		{"privileged", "生命周期", func(f *fakeDocker, _ Config) { f.c.HostConfig.Privileged = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, f, cfg := fixture(t)
			tc.change(f, cfg)
			report, _ := h.check(context.Background(), cfg, "alice", "alice")
			if report.OK {
				t.Fatal("accepted unsafe container")
			}
			found := false
			for _, c := range report.Checks {
				if !c.OK && c.Name == tc.want && c.Reason != "" {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing reason %s: %+v", tc.want, report)
			}
			rows, _ := h.records()
			if len(rows) != 0 {
				t.Fatal("failed check persisted a record")
			}
		})
	}
}
func TestManagedIdentityAndPermission(t *testing.T) {
	for _, change := range []string{"daemon", "config", "replacement", "missing", "viewer"} {
		t.Run(change, func(t *testing.T) {
			h, f, _ := fixture(t)
			adopt(t, h)
			id := f.c.ID
			user := admin
			switch change {
			case "daemon":
				f.daemonID = "other"
			case "config":
				f.c.Name = "/renamed"
			case "replacement":
				f.c.ID = strings.Repeat("b", 64)
			case "missing":
				f.absent = true
			case "viewer":
				user.Role = "viewer"
			}
			f.calls = nil
			status, body := call(h, "POST", "/api/containers/"+id+"/stop", `{}`, user)
			if status == 200 {
				t.Fatal(body)
			}
			for _, args := range f.calls {
				if args[0] == "stop" {
					t.Fatal("unsafe mutation")
				}
			}
			if change != "viewer" {
				status, body = call(h, "POST", "/api/containers/"+id+"/release", `{"confirm":"alice"}`, admin)
				if status != 200 {
					t.Fatal(body)
				}
			}
		})
	}
}
func TestCreatePasswordAndFailureRecovery(t *testing.T) {
	h, f, cfg := fixture(t)
	f.absent = true
	req := CreateRequest{Name: "bob", Port: 32189, Password: "quote'$(danger)123", Network: "host", GPUs: "2"}
	value, err := h.create(context.Background(), cfg, req, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if value["password"] != req.Password {
		t.Fatal("password not returned")
	}
	rows, _ := h.records()
	if len(rows) != 1 || !rows[0].Initialized {
		t.Fatalf("not initialized: %+v", rows)
	}
	found := false
	for i, args := range f.calls {
		if strings.Contains(strings.Join(args, " "), req.Password) {
			t.Fatal("password passed as argument")
		}
		if strings.HasPrefix(f.inputs[i], "root:") {
			found = true
		}
		if args[0] == "create" {
			if !slices.Contains(args, "--gpus") || !slices.Contains(args, "--mount") || !slices.Contains(args, "never") {
				t.Fatalf("missing creation settings: %v", args)
			}
		}
	}
	if !found {
		t.Fatal("password not sent on stdin")
	}
	var raw string
	h.db.SQL.QueryRow("SELECT spec FROM managed_containers").Scan(&raw)
	if strings.Contains(raw, req.Password) {
		t.Fatal("stored password")
	}
	h2, f2, cfg2 := fixture(t)
	f2.absent = true
	f2.failAction = "exec"
	req.Name = "charlie"
	_, err = h2.create(context.Background(), cfg2, req, "admin")
	if err == nil || !strings.Contains(err.Error(), "重试初始化") {
		t.Fatal(err)
	}
	rows, _ = h2.records()
	if len(rows) != 1 || rows[0].Initialized {
		t.Fatalf("lost recovery record: %+v", rows)
	}
	f2.failAction = ""
	status, body := call(h2, "POST", "/api/containers/"+f2.c.ID+"/initialize", `{}`, admin)
	if status != 200 {
		t.Fatal(body)
	}
}
func TestCreateRejectsExistingDataAndInvalidValues(t *testing.T) {
	h, f, cfg := fixture(t)
	f.absent = true
	_, err := h.create(context.Background(), cfg, CreateRequest{Name: "alice", Port: 32189}, "admin")
	if err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatal(err)
	}
	for _, args := range f.calls {
		if args[0] == "create" {
			t.Fatal("created over existing data")
		}
	}
	for _, body := range []string{`{"name":"../escape"}`, `{"name":"bob","port":70000}`, `{"name":"bob","gpus":"0"}`, `{"name":"bob","password":"bad\npass"}`, `{"name":"bob","unknown":true}`} {
		status, value := call(h, "POST", "/api/containers", body, admin)
		if status == 201 {
			t.Fatal(value)
		}
	}
}

func TestHostAdoptionAndSymlinkRejection(t *testing.T) {
	h, f, cfg := fixture(t)
	f.c.HostConfig.NetworkMode = "host"
	f.c.HostConfig.PortBindings = nil
	f.sshPort = 2233
	report, _ := h.check(context.Background(), cfg, "alice", "alice")
	if !report.OK || report.Spec.Port != 2233 {
		t.Fatalf("host adoption: %+v", report)
	}
	path := filepath.Join(cfg.BaseDir, "alice", "home")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), path); err != nil {
		t.Fatal(err)
	}
	report, _ = h.check(context.Background(), cfg, "alice", "alice")
	if report.OK {
		t.Fatal("adopted symlink")
	}
}

func TestPlatformAuthenticationAndCSRF(t *testing.T) {
	h, _, _ := fixture(t)
	server := platform.NewServer(h.db, h, nil, nil, false)
	request := func(method, path, body, csrf string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", csrf)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/api/containers", "", "", nil); w.Code != 401 {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
	w := request("POST", "/api/setup", `{"username":"administrator","password":"test-password-1234"}`, "", nil)
	if w.Code != 200 {
		t.Fatalf("setup: %s", w.Body.String())
	}
	var session platform.Session
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	cookie := w.Result().Cookies()[0]
	w = request("PUT", "/api/containers/settings", `{}`, "", cookie)
	if w.Code != 403 {
		t.Fatalf("missing csrf: %d %s", w.Code, w.Body.String())
	}
	cfg, _ := h.config()
	raw, _ := json.Marshal(cfg)
	w = request("PUT", "/api/containers/settings", string(raw), session.CSRF, cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"endpoint":`) {
		t.Fatalf("authenticated settings: %d %s", w.Code, w.Body.String())
	}
}

func TestCreateCanBeReadopted(t *testing.T) {
	h, f, cfg := fixture(t)
	f.absent = true
	_, err := h.create(context.Background(), cfg, CreateRequest{Name: "bob", Port: 32190, Network: "host"}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	status, body := call(h, "POST", "/api/containers/"+f.c.ID+"/release", `{"confirm":"bob"}`, admin)
	if status != 200 {
		t.Fatal(body)
	}
	report, _ := h.check(context.Background(), cfg, "bob", "bob")
	if !report.OK || report.Spec.Port != 32190 {
		t.Fatalf("cannot readopt: %+v", report)
	}
}

func TestConnectionConfigValidation(t *testing.T) {
	for _, host := range []string{"", "host.example", "127.0.0.1", "[::1]", "2001:db8::1"} {
		c := defaults()
		c.SSHHost = host
		c.ProxyJump = "user@jump.example:2222,user@[::1]"
		if err := c.validate(); err != nil {
			t.Fatalf("valid connection %q: %v", host, err)
		}
	}
	for _, host := range []string{"host;id", "$(id)", "-option", "host name", "host\nother"} {
		c := defaults()
		c.SSHHost = host
		if c.validate() == nil {
			t.Fatalf("accepted %q", host)
		}
	}
}

func TestFingerprintIgnoresMountOrderButDetectsChanges(t *testing.T) {
	h, f, cfg := fixture(t)
	first, err := h.inspect(context.Background(), cfg.Endpoint, "alice")
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(f.c.Mounts)
	second, err := h.inspect(context.Background(), cfg.Endpoint, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint(first) != fingerprint(second) {
		t.Fatal("mount ordering changed identity")
	}
	f.c.Mounts[0].RW = false
	third, err := h.inspect(context.Background(), cfg.Endpoint, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint(second) == fingerprint(third) {
		t.Fatal("mount permission change was ignored")
	}
}
