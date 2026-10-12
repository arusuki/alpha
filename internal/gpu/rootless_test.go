package gpu

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRootlessProcessEvidence(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	uid := u.Uid
	if uid == "0" {
		uid = "1008"
	}
	label := rootlessOwner + " · uid:" + uid
	if account, err := user.LookupId(uid); err == nil {
		label = rootlessOwner + " · " + account.Username
		if account.Username == "docker-rootless" {
			label = rootlessOwner
		}
	}
	id := strings.Repeat("a", 64)
	cg := "0::/user.slice/user-" + uid + ".slice/user@" + uid + ".service/app.slice/docker-" + id + ".scope\n"
	for _, tc := range []struct{ name, cgroup, mapping, want string }{
		{"personal daemon", cg, "0 " + uid + " 1\n1 100000 65536\n", label},
		{"cgroup v1", "3:memory:/docker/" + id + "\n0::/user.slice/user-" + uid + ".slice/user@" + uid + ".service/docker-rootless.service\n", "0 " + uid + " 1\n1 100000 65536\n", label},
		{"rootful", "0::/system.slice/docker-" + id + ".scope\n", "0 0 4294967295\n", ""},
		{"rootful remap", cg, "0 100000 65536\n", ""},
		{"wrong account", cg, "0 2345678 1\n", ""},
		{"host namespace", cg, "0 0 4294967295\n", ""},
		{"unreadable map", cg, "", ""},
		{"other runtime", strings.ReplaceAll(cg, "docker-", "cri-containerd-"), "0 " + uid + " 1\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "10")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			fields := strings.Fields("S 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 4500")
			files := map[string]string{"stat": "10 (train) " + strings.Join(fields, " "), "cgroup": tc.cgroup}
			if tc.mapping != "" {
				files["uid_map"] = tc.mapping
			}
			for name, data := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			p := Process{PID: 10, Owner: "未识别"}
			readProcess(root, &p, 1000)
			want := tc.want
			if want == "" {
				want = "未归属容器"
			}
			if p.Owner != want || p.ContainerID != id {
				t.Fatalf("got %+v, want owner %q", p, want)
			}
		})
	}
}

func TestRootlessInventoryAndRegisteredOwner(t *testing.T) {
	a, b, c := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	socket := filepath.Join(t.TempDir(), "control.sock")
	if err := os.WriteFile(socket, nil, 0600); err != nil {
		t.Fatal(err)
	}
	devices := []Device{{Processes: []Process{{ContainerID: a, Owner: "未归属容器"}, {ContainerID: a, Owner: "未归属容器"}, {ContainerID: b, Owner: "未归属容器"}, {ContainerID: c, Owner: "未归属容器"}}}}
	names := map[string]string{c: "managed-name"}
	calls := 0
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("missing command timeout")
		}
		if name != "docker" {
			t.Fatal(name)
		}
		if calls == 1 {
			return []byte(c + "\tother-name\n"), nil
		}
		if !reflect.DeepEqual(args, []string{"--host", "unix://" + socket, "ps", "--no-trunc", "--format", "{{.ID}}\t{{.Names}}"}) {
			t.Fatal(args)
		}
		return []byte(a + "\ttraining-a\n" + b + "\ttraining-b\n"), nil
	}
	if warning := readContainerNames(context.Background(), devices, names, socket, run); warning != "" {
		t.Fatal(warning)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	applyContainerOwners(devices, names, map[string]string{b: "alice", c: "bob"})
	p := devices[0].Processes
	if p[0].Owner != rootlessOwner || p[0].Container != "training-a" || p[2].Owner != "alice" || p[3].Container != "managed-name" {
		t.Fatal(p)
	}
	if got := deviceOwners(devices[0]); !reflect.DeepEqual(got, []string{"alice", "bob", rootlessOwner}) {
		t.Fatal(got)
	}
	m := testMonitor(t)
	m.previous = Snapshot{At: 1000, Devices: []Device{{UUID: "GPU-a", Processes: []Process{p[0], p[1], {Owner: rootlessOwner, Container: "another-job"}}}}}
	if err := m.persist(Snapshot{At: 1015, Devices: []Device{{UUID: "GPU-a"}}}); err != nil {
		t.Fatal(err)
	}
	var owners string
	if err := m.db.SQL.QueryRow("SELECT owners FROM gpu_intervals").Scan(&owners); err != nil {
		t.Fatal(err)
	}
	if owners != `["rootless Docker"]` {
		t.Fatal(owners)
	}
}

func TestRootlessInventoryUnavailable(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "control.sock")
	devices := []Device{{Processes: []Process{{Owner: rootlessOwner, ContainerID: strings.Repeat("a", 64)}}}}
	calls := 0
	run := func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("unavailable")
	}
	warning := readContainerNames(context.Background(), devices, map[string]string{}, socket, run)
	if calls != 1 || strings.Contains(warning, rootlessOwner) {
		t.Fatal(calls, warning)
	}
	if err := os.WriteFile(socket, nil, 0600); err != nil {
		t.Fatal(err)
	}
	warning = readContainerNames(context.Background(), devices, map[string]string{}, socket, run)
	if calls != 3 || !strings.Contains(warning, rootlessOwner) || devices[0].Processes[0].Owner != rootlessOwner {
		t.Fatal(calls, warning, devices)
	}
}
