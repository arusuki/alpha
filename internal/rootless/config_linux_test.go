package rootless

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testManager(t *testing.T) (*manager, account) {
	t.Helper()
	u := account{Home: t.TempDir(), UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	bin := t.TempDir()
	writeTestFile(t, bin+"/dockerd-rootless.sh", "#!/bin/sh\nexit 0\n", 0755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	m := newManager()
	m.out = &bytes.Buffer{}
	m.errOut = &bytes.Buffer{}
	m.run = func([]string, []string, time.Duration, bool) (result, error) { return result{}, nil }
	return m, u
}
func writeTestFile(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}
func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func mustPrepare(t *testing.T, m *manager, u account, o options) {
	t.Helper()
	if err := m.prepareFiles(u, o); err != nil {
		t.Fatal(err)
	}
}
func readConfig(t *testing.T, u account) map[string]any {
	t.Helper()
	o, err := readObject(layout(u).Config+"/daemon.json", false)
	if err != nil {
		t.Fatal(err)
	}
	return o
}
func saveConfig(t *testing.T, u account, obj map[string]any) {
	t.Helper()
	data, err := jsonBytes(obj)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, layout(u).Config+"/daemon.json", string(data), 0600)
}

func TestSubordinateIDs(t *testing.T) {
	for _, tt := range []struct {
		text        string
		occupied    []uint64
		start       uint64
		needed, bad bool
	}{
		{"docker-rootless:231072:65536\n", nil, 0, false, false}, {"1001:231072:65536\n", nil, 0, false, false},
		{"other:100000:65536\nthird:200000:65536\n", []uint64{265540}, 265541, true, false},
		{"docker-rootless:100000:100\n", nil, 100100, true, false}, {"# comment\n", nil, 100000, true, false},
		{"broken", nil, 0, false, true}, {"x:-1:65536", nil, 0, false, true}, {"x:0:0", nil, 0, false, true}, {"x:0:4294967295", nil, 0, false, true},
	} {
		start, needed, err := subidRange(tt.text, accountName, 1001, tt.occupied)
		if (err != nil) != tt.bad || start != tt.start || needed != tt.needed {
			t.Errorf("%q: %d %v %v", tt.text, start, needed, err)
		}
	}
}
func TestConfigurationPreservesIsolationAndCustomSettings(t *testing.T) {
	m, u := testManager(t)
	mustPrepare(t, m, u, options{})
	p := layout(u)
	cfg := readConfig(t, u)
	for _, key := range []string{"data-root", "exec-root", "pidfile"} {
		if !strings.HasPrefix(cfg[key].(string), u.Home+"/") {
			t.Fatal(key, cfg[key])
		}
	}
	if !reflect.DeepEqual(cfg["hosts"], []any{"unix://" + p.Socket}) || cfg["rootless"] != true {
		t.Fatal(cfg)
	}
	cfg["registry-mirrors"] = []any{"https://mirror.example"}
	cfg["log-driver"] = "json-file"
	saveConfig(t, u, cfg)
	mustPrepare(t, m, u, options{})
	if !reflect.DeepEqual(cfg, readConfig(t, u)) {
		t.Fatal("init lost custom settings")
	}
	unit := readTestFile(t, p.Unit)
	for _, want := range []string{"Delegate=yes", "KillMode=mixed", "StandardError=append:" + p.Log, "Requires=dbus.socket", "UMask=0077"} {
		if !strings.Contains(unit, want) {
			t.Fatal(want)
		}
	}
	for _, key := range []string{"data-root", "exec-root", "pidfile", "hosts", "rootless", "group", "containerd"} {
		t.Run(key, func(t *testing.T) {
			bad := daemonConfig(u)
			bad[key] = "external"
			saveConfig(t, u, bad)
			before := readTestFile(t, p.Config+"/daemon.json")
			if err := m.prepareFiles(u, options{}); err == nil {
				t.Fatal("accepted modified isolation")
			}
			if readTestFile(t, p.Config+"/daemon.json") != before {
				t.Fatal("overwrote invalid config")
			}
		})
	}
}
func TestConfigurationRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"directory-symlink", "broken-symlink", "unmanaged-unit", "exclusive-temp", "invalid-json", "invalid-network", "null-network"} {
		t.Run(kind, func(t *testing.T) {
			m, u := testManager(t)
			p := layout(u)
			if kind == "directory-symlink" || kind == "broken-symlink" {
				target := u.Home
				if kind == "broken-symlink" {
					target += "/missing"
				}
				if err := os.Symlink(target, p.Base); err != nil {
					t.Fatal(err)
				}
			} else {
				mustPrepare(t, m, u, options{})
				switch kind {
				case "unmanaged-unit":
					writeTestFile(t, p.Unit, "[Service]\nExecStart=/bin/true\n", 0600)
				case "exclusive-temp":
					if err := os.Symlink(p.Unit, p.Config+"/daemon.json.tmp"); err != nil {
						t.Fatal(err)
					}
				case "invalid-json":
					writeTestFile(t, p.Config+"/daemon.json", "[]", 0600)
				case "invalid-network":
					writeTestFile(t, p.Config+"/rootlesskit.json", `{"allow-host-loopback":1}`, 0600)
				case "null-network":
					writeTestFile(t, p.Config+"/rootlesskit.json", "null", 0600)
				}
			}
			if err := m.prepareFiles(u, options{}); err == nil {
				t.Fatal("accepted unsafe file")
			}
			if kind == "exclusive-temp" {
				if st, err := os.Lstat(p.Config + "/daemon.json.tmp"); err != nil || st.Mode()&os.ModeSymlink == 0 {
					t.Fatal("removed someone else's temp file")
				}
			}
			if kind == "unmanaged-unit" && readTestFile(t, p.Unit) != "[Service]\nExecStart=/bin/true\n" {
				t.Fatal("overwrote unmanaged unit")
			}
		})
	}
}
func TestProxyUpdatesAndLoopbackPersistence(t *testing.T) {
	m, u := testManager(t)
	mustPrepare(t, m, u, options{})
	allow := true
	mustPrepare(t, m, u, options{Proxies: map[string]string{"http-proxy": "http://user:p%40ss@proxy.example:7890", "https-proxy": "http://proxy.example:7890"}, AllowLoopback: &allow})
	mustPrepare(t, m, u, options{Proxies: map[string]string{"no-proxy": "localhost,.internal"}})
	mustPrepare(t, m, u, options{})
	cfg := readConfig(t, u)
	proxies := cfg["proxies"].(map[string]any)
	if proxies["https-proxy"] != "http://proxy.example:7890" || proxies["no-proxy"] != "localhost,.internal" {
		t.Fatal(cfg)
	}
	var display bytes.Buffer
	if err := showProxy(u, &display); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(display.String(), "user") || strings.Contains(display.String(), "p%40ss") || !strings.Contains(display.String(), "***@proxy.example:7890") {
		t.Fatal(display.String())
	}
	mustPrepare(t, m, u, options{Proxies: map[string]string{"http-proxy": ""}})
	if readConfig(t, u)["proxies"].(map[string]any)["http-proxy"] != "" {
		t.Fatal("empty update ignored")
	}
	mustPrepare(t, m, u, options{Clear: true})
	if _, ok := readConfig(t, u)["proxies"]; ok {
		t.Fatal("clear kept proxies")
	}
	if b, err := networkSettings(u); err != nil || !b {
		t.Fatal("clear lost loopback setting", err)
	}
	if !strings.Contains(readTestFile(t, layout(u).Base+"/launch.sh"), "DOCKERD_ROOTLESS_ROOTLESSKIT_DISABLE_HOST_LOOPBACK=false") {
		t.Fatal("launcher lost opt-in")
	}
	allow = false
	mustPrepare(t, m, u, options{AllowLoopback: &allow})
	if b, err := networkSettings(u); err != nil || b {
		t.Fatal(b, err)
	}
	for _, path := range []string{layout(u).Config + "/daemon.json", layout(u).Config + "/rootlesskit.json", layout(u).Unit} {
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatal(path, st, err)
		}
	}
}
func TestFailedProxyValidationPreservesFilesAndCredentials(t *testing.T) {
	m, u := testManager(t)
	mustPrepare(t, m, u, options{})
	p := layout(u)
	before := map[string]string{}
	for _, path := range []string{p.Config + "/daemon.json", p.Config + "/rootlesskit.json", p.Base + "/launch.sh", p.Unit} {
		before[path] = readTestFile(t, path)
	}
	m.run = func(args, env []string, d time.Duration, check bool) (result, error) {
		if !reflect.DeepEqual(args[:3], []string{"dockerd", "--validate", "--config-file"}) || check {
			t.Fatal(args, check)
		}
		return result{Code: 1, Err: "secret-credentials"}, nil
	}
	allow := true
	err := m.prepareFiles(u, options{Proxies: map[string]string{"http-proxy": "http://secret-credentials@proxy.example"}, AllowLoopback: &allow})
	if err == nil || strings.Contains(err.Error(), "secret-credentials") {
		t.Fatal(err)
	}
	for path, want := range before {
		if readTestFile(t, path) != want {
			t.Fatal("changed", path)
		}
	}
	if _, err = os.Stat(p.Config + "/daemon.json.tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
func TestGeneratedLauncherLockAndRuntimeCleanup(t *testing.T) {
	m, u := testManager(t)
	mustPrepare(t, m, u, options{})
	p := layout(u)
	bin := t.TempDir()
	writeTestFile(t, bin+"/rootlesskit", "#!/bin/sh\nexec \"$@\"\n", 0755)
	wrapper := bin + "/dockerd-rootless.sh"
	writeTestFile(t, wrapper, "#!/bin/sh\nenv > \"$HOME/ready\"\nexec sleep 30\n", 0755)
	launch := launchText(u, false, wrapper)
	launch = strings.Replace(launch, "export PATH="+systemPath, "export PATH="+shellQuote(bin+":"+systemPath), 1)
	writeTestFile(t, p.Base+"/launch.sh", launch, 0700)
	if out, err := exec.Command("sh", "-n", p.Base+"/launch.sh").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	writeTestFile(t, p.Run+"/stale", "old", 0600)
	writeTestFile(t, p.Data+"/preserve", "keep", 0600)
	cmd := exec.Command(p.Base + "/launch.sh")
	cmd.Env = append(os.Environ(), "HTTP_PROXY=http://inherited.invalid", "https_proxy=http://inherited.invalid")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(u.Home + "/ready"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("launcher did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(p.Run + "/stale"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale state kept", err)
	}
	if readTestFile(t, p.Data+"/preserve") != "keep" {
		t.Fatal("data deleted")
	}
	writeTestFile(t, p.Run+"/live", "live", 0600)
	out, err := exec.Command(p.Base + "/launch.sh").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "already running") {
		t.Fatal(err, string(out))
	}
	if readTestFile(t, p.Run+"/live") != "live" {
		t.Fatal("live daemon runtime deleted")
	}
	env := readTestFile(t, u.Home+"/ready")
	for _, bad := range []string{"HTTP_PROXY=", "https_proxy="} {
		if strings.Contains(env, bad) {
			t.Fatal("inherited proxy", env)
		}
	}
	for _, want := range []string{"DOCKERD_ROOTLESS_ROOTLESSKIT_DISABLE_HOST_LOOPBACK=true", "XDG_RUNTIME_DIR=" + p.Run, "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/"} {
		if !strings.Contains(env, want) {
			t.Fatal(want, env)
		}
	}
}
func TestInstalledConfigValidators(t *testing.T) {
	if os.Getenv("ROOTLESS_INTEGRATION") != "1" {
		t.Skip("set ROOTLESS_INTEGRATION=1 to validate with installed dockerd/systemd")
	}
	m, u := testManager(t)
	m.run = runCommand
	allow := true
	mustPrepare(t, m, u, options{Proxies: map[string]string{"http-proxy": "http://user:p%40ss@proxy.example:7890", "https-proxy": "http://proxy.example:7890", "no-proxy": "localhost,127.0.0.1,.internal"}, AllowLoopback: &allow})
	p := layout(u)
	// The production supervisor is installed only when the daemon starts. Use
	// this executable for systemd's path validation without installing anything.
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, p.Unit, strings.ReplaceAll(readTestFile(t, p.Unit), supervisorPath, binary), 0600)
	for _, cmd := range [][]string{{"sh", "-n", p.Base + "/launch.sh"}, {"dockerd", "--validate", "--config-file", p.Config + "/daemon.json"}, {"systemd-analyze", "--user", "verify", p.Unit}} {
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil || strings.Contains(string(out), "Operation not permitted") {
			t.Fatalf("%s: %v %s", cmd[0], err, out)
		}
	}
}
func TestAtomicWriteDoesNotFollowFinalSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	writeTestFile(t, target, "preserve", 0600)
	dest := filepath.Join(dir, "dest")
	if err := os.Symlink(target, dest); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(dest, []byte("new"), 0600, nil); err != nil {
		t.Fatal(err)
	}
	if readTestFile(t, target) != "preserve" || readTestFile(t, dest) != "new" {
		t.Fatal("wrote through symlink")
	}
}
