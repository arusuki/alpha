package storage

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanupDockerProcess(t *testing.T) {
	at := -1
	for i, a := range os.Args {
		if a == "--" {
			at = i
			break
		}
	}
	if at < 0 {
		return
	}
	mode, operation, upper := os.Args[at+1], os.Args[at+2], os.Args[at+3]
	if operation == "inspect" {
		gotUpper := upper
		if mode == "changed" {
			gotUpper += "-changed"
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"id": strings.Repeat("a", 64), "upper": gotUpper, "running": mode != "stopped", "paused": mode == "paused", "readonly": mode == "readonly"})
		os.Exit(0)
	}
	reader := bufio.NewReader(os.Stdin)
	reader.ReadString('\n')
	if mode == "unavailable" {
		fmt.Fprintln(os.Stderr, "python3 not found")
		os.Exit(127)
	}
	if mode == "mounted" {
		fmt.Fprintln(os.Stdout, `{"type":"error","error":"protected mount"}`)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, `{"type":"prepared"}`)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			os.WriteFile(upper+".closed", nil, 0600)
			os.Exit(0)
		}
		if mode == "cancel" {
			continue
		}
		var request struct {
			Index int `json:"index"`
		}
		json.Unmarshal([]byte(line), &request)
		json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "result", "index": request.Index, "skipped_sockets": 1, "skipped_char_devices": 2})
	}
}
func fakeCleanupDocker(t *testing.T, mode, upper string) cleanupCommand {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "docker" || len(args) < 3 || args[0] != "--host" || args[1] != "unix:///var/run/docker.sock" {
			t.Fatalf("unexpected docker command %s %q", name, args)
		}
		op := args[2]
		if op == "exec" {
			want := []string{"exec", "--interactive", "--user", "0:0", "--workdir", "/", strings.Repeat("a", 64), "python3", "-I", "-S", "-u", "-c", cleanupContainerProgram}
			if strings.Join(args[2:], "\x00") != strings.Join(want, "\x00") {
				t.Fatalf("unsafe exec arguments: %q", args[:len(args)-1])
			}
		} else if op != "inspect" {
			t.Fatal(op)
		}
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCleanupDockerProcess$", "--", mode, op, upper)
	}
}
func TestCleanupDockerPreflightAndResults(t *testing.T) {
	for _, mode := range []string{"ok", "changed", "stopped", "paused", "readonly", "unavailable", "mounted"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			upper := filepath.Join(root, "docker", "diff")
			host := filepath.Join(root, "host")
			if err := os.Mkdir(host, 0700); err != nil {
				t.Fatal(err)
			}
			keep := filepath.Join(host, "keep")
			os.WriteFile(keep, nil, 0600)
			req := cleanupHelperRequest{Paths: []string{host, upper + "/tmp", upper + "/root/cache"}, DockerRoot: filepath.Dir(upper), Containers: []cleanupContainer{{ID: strings.Repeat("a", 64), Upper: upper}}}
			targets, err := prepareCleanupTargets(context.Background(), req, nil, fakeCleanupDocker(t, mode, upper))
			if mode != "ok" {
				if err == nil {
					closeCleanupTargets(targets)
					t.Fatal("accepted failed preflight")
				}
				if _, err := os.Stat(keep); err != nil {
					t.Fatal("host deletion ran before container preflight", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer closeCleanupTargets(targets)
			if targets[1].Session != targets[2].Session {
				t.Fatal("worker was not grouped per container")
			}
			stats := new(cleanupStats)
			if err := targets[2].remove(context.Background(), stats); err != nil {
				t.Fatal(err)
			}
			if stats.Sockets != 1 || stats.CharDevices != 2 {
				t.Fatal(stats)
			}
		})
	}
}
func TestCleanupDockerCancellationClosesRemoteStdin(t *testing.T) {
	upper := filepath.Join(t.TempDir(), "diff")
	ctx, cancel := context.WithCancel(context.Background())
	session, err := startCleanupDocker(ctx, cleanupContainer{ID: strings.Repeat("a", 64), Upper: upper}, []string{"/tmp"}, nil, fakeCleanupDocker(t, "cancel", upper))
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	done := make(chan error, 1)
	go func() { done <- session.remove(0, new(cleanupStats)) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("reported cancelled cleanup as successful")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remote worker survived cancellation")
	}
	session.close()
	if _, err := os.Stat(upper + ".closed"); err != nil {
		t.Fatal("worker never received EOF", err)
	}
}

// The opt-in Docker integration harness uses the real host helper protocol with
// disposable fixtures, without asking tests to authenticate through sudo.
func TestCleanupIntegrationProcess(t *testing.T) {
	if os.Getenv("PROJECT_ALPHA_CLEANUP_INTEGRATION_HELPER") != "1" {
		return
	}
	if err := serveCleanupHelper(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
