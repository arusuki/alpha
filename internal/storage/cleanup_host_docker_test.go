package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
)

func TestHostDockerMockProcess(t *testing.T) {
	at := -1
	for i, arg := range os.Args {
		if arg == "--host-docker-mock" {
			at = i
			break
		}
	}
	if at < 0 {
		return
	}
	mode, operation, count := os.Args[at+1], os.Args[at+2], os.Args[at+3]
	if mode == operation+"_error" {
		os.Exit(1)
	}
	if mode == "slow" {
		time.Sleep(time.Second)
	}
	id := strings.Repeat("a", 64)
	switch operation {
	case "info":
		root, id := "/daemon-root", "daemon-01"
		if mode == "root_changed" {
			root = "/another-root"
		}
		if strings.HasPrefix(mode, "root_alias=") {
			root = strings.TrimPrefix(mode, "root_alias=")
		}
		if mode == "id_changed" {
			id = "daemon-02"
		}
		fmt.Fprintln(os.Stdout, httpapi.JSONText(object{"id": id, "root": root}))
	case "ps":
		fmt.Fprintln(os.Stdout, id)
		if mode == "changed" && count == "2" {
			fmt.Fprintln(os.Stdout, strings.Repeat("b", 64))
		}
	case "inspect":
		mount := "/srv/other"
		if mode == "bind" || mode == "volume" {
			mount = "/srv/cache/child"
		}
		fmt.Fprintln(os.Stdout, httpapi.JSONText(object{"id": id, "upper": "/layers/upper", "log_path": "/logs/container.log", "mounts": []object{{"Source": mount}}}))
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func fakeHostDockerCommand(t *testing.T, mode, endpoint string) cleanupCommand {
	t.Helper()
	psCalls := 0
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "docker" || len(args) < 3 || args[0] != "--host" || args[1] != endpoint {
			t.Fatalf("Host Docker preflight used another endpoint: %s %q", name, args)
		}
		op := args[2]
		count := 0
		switch op {
		case "info":
			if len(args) != 5 || args[3] != "--format" || args[4] != `{"id":{{json .ID}},"root":{{json .DockerRootDir}}}` {
				t.Fatalf("unexpected Docker info command: %q", args)
			}
		case "ps":
			if !slices.Equal(args[3:], []string{"--all", "--quiet", "--no-trunc"}) {
				t.Fatalf("stopped containers were omitted: %q", args)
			}
			psCalls++
			count = psCalls
		case "inspect":
			if len(args) != 8 || args[3] != "--type" || args[4] != "container" || args[5] != "--format" || args[7] != strings.Repeat("a", 64) {
				t.Fatalf("unexpected Docker inspect command: %q", args)
			}
		default:
			t.Fatalf("unexpected Docker operation: %q", args)
		}
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHostDockerMockProcess$", "--", "--host-docker-mock", mode, op, strconv.Itoa(count))
	}
}

func TestHostDockerPreflightProtectsLiveSources(t *testing.T) {
	for _, tc := range []struct {
		mode, target string
		status       int
	}{
		{mode: "ok", target: "/srv/cache"},
		{mode: "root", target: "/daemon-root/images", status: 409},
		{mode: "bind", target: "/srv/cache", status: 409},
		{mode: "volume", target: "/srv/cache/child/file", status: 409},
		{mode: "upper", target: "/layers/upper/tmp", status: 409},
		{mode: "log", target: "/logs", status: 409},
		{mode: "info_error", target: "/srv/cache", status: 503},
		{mode: "ps_error", target: "/srv/cache", status: 503},
		{mode: "inspect_error", target: "/srv/cache", status: 503},
		{mode: "changed", target: "/srv/cache", status: 409},
		{mode: "id_changed", target: "/srv/cache", status: 409},
		{mode: "root_changed", target: "/srv/cache", status: 409},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			endpoint := "unix:///run/user/1000/docker.sock"
			protected, err := checkLiveDockerHostPaths(context.Background(), []string{tc.target}, endpoint, "/daemon-root", "daemon-01", fakeHostDockerCommand(t, tc.mode, endpoint))
			if tc.status == 0 {
				if err != nil || !slices.Contains(protected, "/daemon-root") || !slices.Contains(protected, "/srv/other") || !slices.Contains(protected, "/layers/upper") || !slices.Contains(protected, "/logs/container.log") {
					t.Fatalf("live Docker paths were not protected: %v %v", protected, err)
				}
				return
			}
			var apiErr *httpapi.Error
			if !errors.As(err, &apiErr) || apiErr.Status != tc.status {
				t.Fatalf("unsafe or unavailable Docker state accepted: %v %v", protected, err)
			}
		})
	}
}

func TestHostDockerPreflightSocketAndDeadline(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.sock")
	endpoint := "unix://" + missing
	if protected, err := validateLiveDockerHostPaths(context.Background(), []string{"/srv/cache"}, endpoint, "", "", fakeHostDockerCommand(t, "ok", endpoint)); err != nil || len(protected) != 0 {
		t.Fatalf("missing Docker socket should use snapshot protections: %v %v", protected, err)
	}
	if _, err := validateLiveDockerHostPaths(context.Background(), []string{"/srv/cache"}, endpoint, "/daemon-root", "daemon-01", fakeHostDockerCommand(t, "ok", endpoint)); err == nil {
		t.Fatal("Docker snapshot was accepted without a live daemon at the checked socket")
	}
	regular := filepath.Join(t.TempDir(), "docker.sock")
	if err := os.WriteFile(regular, nil, 0600); err != nil {
		t.Fatal(err)
	}
	regularEndpoint := "unix://" + regular
	if _, err := validateLiveDockerHostPaths(context.Background(), []string{"/srv/cache"}, regularEndpoint, "", "", fakeHostDockerCommand(t, "ok", regularEndpoint)); err == nil {
		t.Fatal("non-socket Docker endpoint was accepted")
	}
	if _, err := checkLiveDockerHostPaths(context.Background(), []string{"/srv/cache"}, endpoint, "/other-daemon-root", "daemon-01", fakeHostDockerCommand(t, "ok", endpoint)); err == nil {
		t.Fatal("Host deletion accepted a different Docker daemon than the snapshot")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := checkLiveDockerHostPaths(ctx, []string{"/srv/cache"}, endpoint, "/daemon-root", "daemon-01", fakeHostDockerCommand(t, "slow", endpoint)); err == nil {
		t.Fatal("Docker preflight ignored its deadline")
	}
}

func TestHostDockerPreflightRejectsRetargetedDataRootAlias(t *testing.T) {
	base := t.TempDir()
	oldRoot, newRoot, alias := filepath.Join(base, "old"), filepath.Join(base, "new"), filepath.Join(base, "docker-link")
	for _, path := range []string{oldRoot, newRoot} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(oldRoot, alias); err != nil {
		t.Fatal(err)
	}
	// The snapshot preserved oldRoot while Docker reports the same alias after
	// it was moved to newRoot. Resolving both paths only at deletion would pass.
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(newRoot, alias); err != nil {
		t.Fatal(err)
	}
	endpoint := "unix:///run/user/1000/docker.sock"
	_, err := checkLiveDockerHostPaths(context.Background(), []string{filepath.Join(base, "safe")}, endpoint, oldRoot, "daemon-01", fakeHostDockerCommand(t, "root_alias="+alias, endpoint))
	var apiErr *httpapi.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 409 {
		t.Fatalf("retargeted Docker data root was accepted: %v", err)
	}
}
