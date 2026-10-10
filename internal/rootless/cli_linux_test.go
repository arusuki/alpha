package rootless

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCLIArguments(t *testing.T) {
	for _, args := range [][]string{{"daemon"}, {"proxy", "--http-proxy", ""}, {"proxy", "--clear", "--disable-host-loopback"}, {"add", "container", "--host", "unix:///tmp/docker.sock", "--socket-path", "/run/custom.sock"}, {"add", "--socket-path=/run/custom.sock", "container"}, {"docker", "--", "exec", "-it", "c", "sh"}, {"test", "exec", "my-test"}, {"remove", "container"}, {"daemon", "--host-namespaces", "--socket-gid", "1005"}, {"list"}, {"--help"}, {"add", "--help"}} {
		if _, err := parseOptions(args); err != nil {
			t.Errorf("%q: %v", args, err)
		}
	}
	for _, args := range [][]string{nil, {"unknown"}, {"add"}, {"add", "a", "b"}, {"status", "unexpected"}, {"daemon", "--clear"}, {"proxy", "--clear", "--http-proxy", ""}, {"daemon", "--allow-host-loopback", "--disable-host-loopback"}, {"proxy", "--http-proxy"}, {"add", "c", "--host", "ssh://host"}, {"add", "c", "--socket-path", "/run/../docker.sock"}, {"test", "up", "-invalid"}, {"proxy", "--http-proxy", "http://secret@host:65536"}} {
		if _, err := parseOptions(args); err == nil {
			t.Errorf("accepted %q", args)
		} else if strings.Contains(err.Error(), "secret") {
			t.Fatal("credentials leaked", err)
		}
	}
	o, err := parseOptions([]string{"docker", "--", "exec", "-it", "c", "sh"})
	if err != nil || !reflect.DeepEqual(o.Args, []string{"exec", "-it", "c", "sh"}) {
		t.Fatal(o, err)
	}
	o, err = parseOptions([]string{"proxy"})
	if err != nil || o.AllowLoopback != nil {
		t.Fatal(o, err)
	}
	for _, flag := range []string{"--allow-host-loopback", "--disable-host-loopback"} {
		o, err = parseOptions([]string{"proxy", flag})
		if err != nil || o.AllowLoopback == nil || *o.AllowLoopback != (flag == "--allow-host-loopback") {
			t.Fatal(o, err)
		}
	}
}
func TestProxyArgumentValidation(t *testing.T) {
	for _, s := range []string{"", "http://proxy.example:7890", "https://proxy.example", "http://user:p%40ss@[2001:db8::1]:3128/"} {
		if err := validateProxyURL(s); err != nil {
			t.Fatal(s, err)
		}
	}
	for _, s := range []string{"proxy.example:7890", "http://", "socks5://proxy.example:1080", "http://proxy.example:65536", "http://proxy.example:0", "http://proxy.example\n", "http://proxy.example/?secret=1", "http://proxy.example/#secret", "http://user:secret@[invalid"} {
		if err := validateProxyURL(s); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid URL accepted or leaked", err)
		}
	}
	if err := validateNoProxy("localhost\nOTHER=value"); err == nil {
		t.Fatal("accepted control character")
	}
	for _, s := range []string{"run/docker.sock", "/run/../docker.sock", "/run/", "/run/\x00sock"} {
		if err := validateContainerPath(s); err == nil {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"tcp://localhost:2375", "ssh://host", "unix://relative", "unix:///\x00"} {
		if err := validateHost(s); err == nil {
			t.Fatal(s)
		}
	}
}
func TestDaemonVerificationAndClientEnvironment(t *testing.T) {
	m, u := testManager(t)
	t.Setenv("DOCKER_CONTEXT", "remote")
	t.Setenv("DOCKER_HOST", "tcp://bad:2375")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	m.run = func(args, env []string, d time.Duration, check bool) (result, error) {
		if !reflect.DeepEqual(args[:3], []string{"docker", "--host", testHost}) {
			t.Fatal(args)
		}
		if !reflect.DeepEqual(env, rootDockerEnv) {
			t.Fatal(env)
		}
		return result{}, nil
	}
	if _, err := m.docker(u, testHost, "info"); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		info daemonInfo
		bad  bool
	}{{daemonInfo{}, true}, {daemonInfo{SecurityOptions: []string{"name=rootless"}, DockerRootDir: "/var/lib/docker"}, true}, {daemonInfo{SecurityOptions: []string{"name=rootless"}, DockerRootDir: layout(u).Data}, false}} {
		m.run = func(args, env []string, d time.Duration, check bool) (result, error) {
			if args[0] != "runuser" || !strings.Contains(strings.Join(args, " "), "--config "+layout(u).Client+" --host unix://"+layout(u).Socket) {
				t.Fatal(args)
			}
			b, _ := json.Marshal(tt.info)
			return result{Out: string(b)}, nil
		}
		if err := m.verifyDaemon(u); (err != nil) != tt.bad {
			t.Fatal(tt, err)
		}
	}
}
func TestProxyServiceLifecycle(t *testing.T) {
	for _, kind := range []string{"show", "update", "clear", "loopback", "failed-validation", "failed-restart"} {
		t.Run(kind, func(t *testing.T) {
			m, u := testManager(t)
			mustPrepare(t, m, u, options{})
			var commands [][]string
			var actions []string
			m.worker = func(r workerRequest, asUser bool) error {
				if !asUser {
					t.Fatal("root configuration write")
				}
				actions = append(actions, r.Action)
				if kind == "failed-validation" {
					return errors.New("validation failed")
				}
				return nil
			}
			m.run = func(args, env []string, d time.Duration, check bool) (result, error) {
				commands = append(commands, args)
				if strings.HasSuffix(strings.Join(args, " "), "restart "+serviceName) && kind == "failed-restart" {
					return result{}, errors.New("restart failed")
				}
				if args[len(args)-1] == "{{json .}}" {
					b, _ := json.Marshal(daemonInfo{SecurityOptions: []string{"name=rootless"}, DockerRootDir: layout(u).Data})
					return result{Out: string(b)}, nil
				}
				return result{}, nil
			}
			o := options{}
			switch kind {
			case "update", "failed-validation", "failed-restart":
				o.Proxies = map[string]string{"http-proxy": "http://proxy.example:7890"}
			case "clear":
				o.Clear = true
			case "loopback":
				b := true
				o.AllowLoopback = &b
			}
			err := m.configureProxy(u, o)
			bad := strings.HasPrefix(kind, "failed")
			if (err != nil) != bad {
				t.Fatal(err)
			}
			if kind == "show" {
				if !reflect.DeepEqual(actions, []string{"show-proxy"}) || len(commands) != 0 {
					t.Fatal(actions, commands)
				}
				return
			}
			if !reflect.DeepEqual(actions, []string{"prepare"}) {
				t.Fatal(actions)
			}
			if kind == "failed-validation" {
				if len(commands) != 0 {
					t.Fatal("restarted after failed validation")
				}
				return
			}
			if len(commands) < 2 || !strings.HasSuffix(strings.Join(commands[0], " "), "systemctl --user --no-pager daemon-reload") || !strings.HasSuffix(strings.Join(commands[1], " "), "systemctl --user --no-pager restart "+serviceName) {
				t.Fatal(commands)
			}
			if kind == "failed-restart" && !strings.Contains(m.errOut.(*bytes.Buffer).String(), "配置已保存") {
				t.Fatal("missing failure guidance")
			}
		})
	}
}
func TestServiceRequiresManagerLease(t *testing.T) {
	_, u := testManager(t)
	unit := unitText(u)
	for _, want := range []string{"--internal-supervisor", "/run/rootless-docker/lease.sock", "Restart=no", "KillMode=mixed", "Delegate=yes"} {
		if !strings.Contains(unit, want) {
			t.Fatal("missing lifecycle property", want)
		}
	}
	for _, bad := range []string{"WantedBy=", "Restart=always"} {
		if strings.Contains(unit, bad) {
			t.Fatal("independent service lifetime", bad)
		}
	}
}
func TestTestContainerOwnershipAndCleanup(t *testing.T) {
	for _, kind := range []string{"absent", "owned", "foreign", "daemon-error", "rootless"} {
		t.Run(kind, func(t *testing.T) {
			m, _ := testManager(t)
			var calls []string
			m.run = func(args, env []string, d time.Duration, check bool) (result, error) {
				s := strings.Join(args, " ")
				calls = append(calls, s)
				switch {
				case strings.Contains(s, "info --format"):
					if kind == "rootless" {
						return result{Out: `["name=rootless"]`}, nil
					}
					return result{Out: `[]`}, nil
				case strings.Contains(s, "container ls"):
					if kind == "daemon-error" {
						return result{}, errors.New("daemon unavailable")
					}
					if kind == "absent" {
						return result{Out: "other rootless-cli-test-similar\n"}, nil
					}
					return result{Out: "cid rootless-cli-test\n"}, nil
				case strings.Contains(s, "container inspect"):
					owner := "1"
					if kind == "foreign" {
						owner = "<no value>"
					}
					return result{Out: owner}, nil
				case strings.Contains(s, "rm --force --volumes cid"):
					return result{Out: "cid\n"}, nil
				default:
					t.Fatal(args)
					return result{}, nil
				}
			}
			d := &daemon{manager: m, stateFile: t.TempDir() + "/bindings.json", store: bindingStore{Version: 1, Bindings: []binding{
				{Host: testHost, Container: "cid", Name: "rootless-cli-test", SocketPath: "/sock"},
				{Host: testHost, Container: "other", Name: "other", SocketPath: "/sock"},
				{Host: "unix:///other.sock", Container: "cid", Name: "remote", SocketPath: "/sock"},
			}}}
			if err := d.save(); err != nil {
				t.Fatal(err)
			}
			err := d.testContainer([]string{"cleanup"})
			if (err != nil) != (kind == "foreign" || kind == "daemon-error" || kind == "rootless") {
				t.Fatal(err)
			}
			removed := strings.Contains(strings.Join(calls, "\n"), "rm --force")
			if removed != (kind == "owned") {
				t.Fatal(calls)
			}
			saved, err := loadBindings(d.stateFile)
			if err != nil {
				t.Fatal(err)
			}
			want := 3
			if kind == "owned" {
				want = 2
				if saved.Bindings[0].Container != "other" || saved.Bindings[1].Host != "unix:///other.sock" {
					t.Fatal("cleanup removed another container's binding", saved)
				}
			}
			if len(saved.Bindings) != want || strings.Count(strings.Join(calls, "\n"), "container ls") > 1 {
				t.Fatal("cleanup repeated lookup or lost bindings", calls, saved)
			}
		})
	}
}
func TestRunCommandExitStatusAndTimeout(t *testing.T) {
	r, err := runCommand([]string{"sh", "-c", "printf output; printf diagnostic >&2; exit 7"}, nil, time.Second, false)
	if err != nil || r.Code != 7 || r.Out != "output" || r.Err != "diagnostic" {
		t.Fatal(r, err)
	}
	_, err = runCommand([]string{"sh", "-c", "exec sleep 20"}, nil, 50*time.Millisecond, true)
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatal(err)
	}
}

func TestAddChecksDaemonContainerAndConcurrentRestarts(t *testing.T) {
	for _, kind := range []string{"ok", "rootless-target", "paused", "stopped", "restarting", "invalid-pid", "socket-replaced", "container-restarted", "userns-remap"} {
		t.Run(kind, func(t *testing.T) {
			m, u := testManager(t)
			mustPrepare(t, m, u, options{})
			m.readUIDMap = func(int) ([]byte, error) {
				if kind == "userns-remap" {
					return []byte("0 100000 65536"), nil
				}
				return []byte("0 0 4294967295"), nil
			}
			source := layout(u).Socket
			socketFile(t, source)
			state := containerState{Pid: os.Getpid(), Running: true, StartedAt: "start"}
			switch kind {
			case "paused":
				state.Paused = true
			case "stopped":
				state.Running = false
			case "restarting":
				state.Restarting = true
			case "invalid-pid":
				state.Pid = 1
			}
			attached := false
			inspectCount := 0
			m.worker = func(r workerRequest, asUser bool) error {
				attached = true
				json.NewEncoder(m.out).Encode(mountReceipt{Namespace: 1, Device: 2, Inode: 3, MountID: "4"})
				if asUser || r.Action != "attach" || r.PID != state.Pid || r.Source != source || r.Destination != "/run/custom.sock" {
					t.Fatal(r, asUser)
				}
				if kind == "socket-replaced" {
					if err := os.Rename(source, source+".old"); err != nil {
						t.Fatal(err)
					}
					socketFile(t, source)
				}
				return nil
			}
			m.run = func(args, env []string, d time.Duration, check bool) (result, error) {
				var obj any
				s := strings.Join(args, " ")
				switch {
				case args[0] == "runuser":
					obj = daemonInfo{SecurityOptions: []string{"name=rootless"}, DockerRootDir: layout(u).Data}
				case strings.Contains(s, "info --format"):
					info := daemonInfo{}
					if kind == "rootless-target" {
						info.SecurityOptions = []string{"name=rootless"}
					}
					obj = info
				case strings.Contains(s, "container inspect"):
					inspectCount++
					current := state
					if inspectCount == 2 && kind == "container-restarted" {
						current.StartedAt = "later"
					}
					if inspectCount == 1 && !reflect.DeepEqual(args[len(args)-2:], []string{"--", "-container"}) {
						t.Fatal("container option injection", args)
					}
					if inspectCount == 2 && args[len(args)-1] != "stable-id" {
						t.Fatal("rechecked name instead of ID", args)
					}
					obj = []containerInfo{{Id: "stable-id", State: current}}
				default:
					t.Fatal(args)
				}
				b, _ := json.Marshal(obj)
				return result{Out: string(b)}, nil
			}
			d := &daemon{manager: m, user: u, bootID: "boot", stateFile: t.TempDir() + "/bindings.json", store: bindingStore{Version: 1, Bindings: []binding{}}}
			err := d.add(options{Container: "-container", Host: testHost, SocketPath: "/run/custom.sock"})
			if (err != nil) != (kind != "ok") {
				t.Fatal(kind, err)
			}
			wantAttach := kind == "ok" || kind == "socket-replaced" || kind == "container-restarted"
			if attached != wantAttach {
				t.Fatal("unexpected attach", kind, attached)
			}
			if kind == "ok" && inspectCount != 2 {
				t.Fatal("add should inspect once before and once after mounting", inspectCount)
			}
		})
	}
}
