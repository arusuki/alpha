package containers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

const permissionTestPassword = "test-only-permission-secret"

func TestPermissionSudoProcess(t *testing.T) {
	mode := ""
	for i, arg := range os.Args {
		if arg == "permission-test" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
		}
	}
	if mode == "" {
		return
	}
	r := bufio.NewReader(os.Stdin)
	if mode != "nopass" && mode != "wait" {
		fmt.Fprint(os.Stderr, permissionPrompt[:10])
		fmt.Fprint(os.Stderr, permissionPrompt[10:])
		password, err := r.ReadString('\n')
		if err != nil || password != permissionTestPassword+"\n" {
			os.Exit(2)
		}
		if mode == "bad" {
			fmt.Fprint(os.Stderr, "PAM reflected: "+password+permissionPrompt)
			_, _ = r.ReadString('\n')
			os.Exit(1)
		}
	}
	fmt.Fprintln(os.Stdout, `{"type":"ready"}`)
	raw, err := r.ReadBytes('\n')
	var req permissionRepair
	if err != nil || json.Unmarshal(raw, &req) != nil || bytes.Contains(raw, []byte(permissionTestPassword)) {
		os.Exit(3)
	}
	if mode == "wait" {
		_, _ = io.Copy(io.Discard, r)
		os.Exit(0)
	}
	code := ""
	if mode == "acl" {
		code = "acl"
	}
	_ = json.NewEncoder(os.Stdout).Encode(permissionEvent{Type: "result", Code: code})
	os.Exit(0)
}

func permissionTestCommand(t *testing.T, mode string) func(context.Context, string, ...string) *exec.Cmd {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "/usr/bin/sudo" || len(args) != 7 || args[0] != "-S" || args[1] != "-k" || args[2] != "-p" || args[3] != permissionPrompt || args[4] != "--" || args[6] != "container-permissions-helper" {
			t.Fatalf("unexpected sudo invocation: %s %v", name, args)
		}
		if strings.Contains(strings.Join(args, " "), permissionTestPassword) {
			t.Fatal("secret in argv")
		}
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPermissionSudoProcess$", "--", "permission-test", mode)
	}
}

func TestPermissionSudoSecretAndFailures(t *testing.T) {
	for _, mode := range []string{"password", "nopass", "bad", "acl", "wait"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if mode == "wait" {
				time.AfterFunc(100*time.Millisecond, cancel)
			}
			password := []byte(permissionTestPassword)
			err := runSudoPermissions(ctx, password, permissionRepair{Action: "directory", UID: os.Geteuid(), BaseDir: t.TempDir()}, permissionTestCommand(t, mode))
			if !bytes.Equal(password, make([]byte, len(password))) {
				t.Fatal("secret not cleared")
			}
			if mode == "password" || mode == "nopass" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || strings.Contains(err.Error(), permissionTestPassword) {
				t.Fatalf("failure missing or secret exposed: %v", err)
			}
		})
	}
}

func TestPermissionScopeValidation(t *testing.T) {
	valid := permissionRepair{Action: "directory", UID: 1234, BaseDir: "/docker"}
	if !validPermissionRepair(valid, "1234") {
		t.Fatal("valid request rejected")
	}
	for _, tc := range []struct {
		req  permissionRepair
		sudo string
	}{
		{valid, ""}, {valid, "1235"}, {permissionRepair{Action: "shell", UID: 1234, BaseDir: "/docker"}, "1234"},
		{permissionRepair{Action: "directory", UID: 1234, BaseDir: "/"}, "1234"},
		{permissionRepair{Action: "directory", UID: 1234, BaseDir: "/tmp/../etc"}, "1234"},
	} {
		if validPermissionRepair(tc.req, tc.sudo) {
			t.Fatalf("accepted invalid scope: %+v", tc)
		}
	}
}

func TestPermissionDirectoryProbeAndSymlinks(t *testing.T) {
	path := t.TempDir()
	if err := probeDirectory(path); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(path)
	if len(entries) != 0 {
		t.Fatal("probe left data behind")
	}
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{link, filepath.Join(link, "new"), filepath.Join(path, "missing", "child")} {
		if dir, err := openPermissionDirectory(invalid, true); err == nil {
			dir.Close()
			t.Fatalf("accepted %s", invalid)
		}
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(path, 0500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(path, 0700) })
		if err := probeDirectory(path); err == nil {
			t.Fatal("reported read-only directory writable")
		}
	}
}

func TestPermissionACLUsesPinnedDirectory(t *testing.T) {
	if _, err := os.Stat("/usr/bin/setfacl"); err != nil {
		t.Skip("setfacl unavailable")
	}
	root := t.TempDir()
	target, moved, other := filepath.Join(root, "target"), filepath.Join(root, "moved"), filepath.Join(root, "other")
	for _, path := range []string{target, other} {
		if err := os.Mkdir(path, 0500); err != nil {
			t.Fatal(err)
		}
	}
	command := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "/usr/bin/setfacl" || strings.Join(args, " ") != "--modify u::rwx -- /proc/self/fd/3" {
			t.Fatalf("unexpected ACL command: %s %v", name, args)
		}
		if err := os.Rename(target, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, target); err != nil {
			t.Fatal(err)
		}
		return exec.CommandContext(ctx, name, args...)
	}
	if code := applyPermissionRepair(context.Background(), permissionRepair{Action: "directory", UID: os.Geteuid(), BaseDir: target}, command); code != "" {
		t.Fatal(code)
	}
	info, _ := os.Stat(moved)
	if info.Mode().Perm() != 0700 {
		t.Fatal("pinned directory was not authorized")
	}
	info, _ = os.Stat(other)
	if info.Mode().Perm() != 0500 {
		t.Fatal("followed replacement symlink")
	}
}

func TestPermissionCreateDirectoryAndPreserveData(t *testing.T) {
	if _, err := os.Stat("/usr/bin/setfacl"); err != nil {
		t.Skip("setfacl unavailable")
	}
	path := filepath.Join(t.TempDir(), "new")
	req := permissionRepair{Action: "directory", UID: os.Geteuid(), BaseDir: path}
	for i := 0; i < 2; i++ {
		if code := applyPermissionRepair(context.Background(), req, exec.CommandContext); code != "" {
			t.Fatal(code)
		}
		if err := probeDirectory(path); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(path, "existing")
		if i == 0 {
			if err := os.WriteFile(file, []byte("keep"), 0400); err != nil {
				t.Fatal(err)
			}
		} else {
			raw, err := os.ReadFile(file)
			info, _ := os.Stat(file)
			if err != nil || string(raw) != "keep" || info.Mode().Perm() != 0400 {
				t.Fatal("existing data modified")
			}
		}
	}
}

func TestPermissionNamedACL(t *testing.T) {
	for _, path := range []string{"/usr/bin/setfacl", "/usr/bin/getfacl"} {
		if _, err := os.Stat(path); err != nil {
			t.Skip("ACL tools unavailable")
		}
	}
	u, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("nobody account unavailable")
	}
	uid, _ := strconv.Atoi(u.Uid)
	if uid == os.Geteuid() {
		t.Skip("need a distinct ACL identity")
	}
	path := t.TempDir()
	if code := applyPermissionRepair(context.Background(), permissionRepair{Action: "directory", UID: uid, BaseDir: path}, exec.CommandContext); code != "" {
		t.Fatal(code)
	}
	raw, err := exec.Command("/usr/bin/getfacl", "--omit-header", "--numeric", "--", path).Output()
	if err != nil || !strings.Contains(string(raw), "user:"+u.Uid+":rwx") {
		t.Fatalf("named ACL not applied: %s %v", raw, err)
	}
}

func TestPermissionGroupAppendAndActiveStatus(t *testing.T) {
	u, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	group := &user.Group{Gid: u.Gid, Name: "test-primary"}
	gid, _ := strconv.Atoi(u.Gid)
	member, active, err := groupMembership(u, group, nil, -1)
	if err != nil || !member || active {
		t.Fatalf("persisted vs process groups: %v %v %v", member, active, err)
	}
	_, active, _ = groupMembership(u, group, []int{gid}, -1)
	if !active {
		t.Fatal("supplementary group not recognized")
	}
	_, active, _ = groupMembership(u, group, nil, gid)
	if !active {
		t.Fatal("primary group not recognized")
	}
	if os.Geteuid() == 0 {
		u, err = user.LookupId("65534")
		if err != nil {
			t.Skip("non-root account unavailable")
		}
	}
	uid, _ := strconv.Atoi(u.Uid)
	var calls []string
	command := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return exec.CommandContext(ctx, "/bin/true")
	}
	if code := applyPermissionRepair(context.Background(), permissionRepair{Action: "docker_group", UID: uid, BaseDir: "/docker"}, command); code != "" {
		t.Fatal(code)
	}
	if len(calls) != 2 || calls[0] != "/usr/sbin/groupadd --force docker" || calls[1] != "/usr/sbin/usermod --append --groups docker -- "+u.Username {
		t.Fatal(calls)
	}
}

func TestPermissionAPIAdminScopeAndSecret(t *testing.T) {
	h, docker, cfg := fixture(t)
	docker.failAction = "info" // Inspection and sudo repair must work without Docker access.
	h.permissionCommand = permissionTestCommand(t, "password")
	call := func(h *Handler, method, path, body string, actor platform.User) (int, string) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		status, value, err := h.Dispatch(httptest.NewRecorder(), r, actor)
		if err != nil {
			var apiErr *httpapi.Error
			if errors.As(err, &apiErr) {
				return apiErr.Status, apiErr.Message
			}
			return 500, err.Error()
		}
		raw, _ := json.Marshal(value)
		return status, string(raw)
	}
	for _, method := range []string{"GET", "POST"} {
		if status, _ := call(h, method, "/api/containers/permissions", `{}`, platform.User{Role: "viewer"}); status != 403 {
			t.Fatal(status)
		}
	}
	if status, body := call(h, "GET", "/api/containers/permissions", "", admin); status != 200 || !strings.Contains(body, `"directory_writable":true`) || !strings.Contains(body, `"docker_available":false`) {
		t.Fatalf("%d %s", status, body)
	}
	request := map[string]string{"action": "directory", "base_dir": cfg.BaseDir, "endpoint": cfg.Endpoint, "sudo_password": permissionTestPassword}
	for _, mode := range []string{"stale", "invalid", "ok"} {
		r := make(map[string]string)
		for k, v := range request {
			r[k] = v
		}
		if mode == "stale" {
			r["base_dir"] = "/other"
		}
		if mode == "invalid" {
			r["username"] = "root"
		}
		raw, _ := json.Marshal(r)
		status, body := call(h, "POST", "/api/containers/permissions", string(raw), admin)
		want := 200
		if mode == "stale" {
			want = 409
		}
		if mode == "invalid" {
			want = 400
		}
		if status != want || strings.Contains(body, permissionTestPassword) {
			t.Fatalf("%s: %d %s", mode, status, body)
		}
	}
	rows, err := h.db.SQL.Query("SELECT detail FROM audit WHERE action LIKE 'container.permissions.%'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, permissionTestPassword) {
			t.Fatal("audit retained secret")
		}
	}
}
