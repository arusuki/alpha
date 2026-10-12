package gpu

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const rootlessOwner = "rootless Docker"

var userSlicePattern = regexp.MustCompile(`(?:^|/)user-([0-9]+)\.slice/`)

// A user namespace alone is not evidence of rootless Docker: rootful Docker
// also supports userns-remap. Require a Docker cgroup under that user's slice
// and the single-UID root mapping used by rootless Docker.
func rootlessProcessOwner(dir, cgroup string) string {
	if !strings.Contains(cgroup, "docker") {
		return ""
	}
	match := userSlicePattern.FindStringSubmatch(cgroup)
	if match == nil || match[1] == "0" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(dir, "uid_map"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[0] != "0" || f[1] != match[1] || f[2] != "1" {
			continue
		}
		// The shared service account identifies the engine, not the person who
		// submitted the workload through one of its mounted sockets.
		if u, err := user.LookupId(match[1]); err == nil {
			if u.Username == "docker-rootless" {
				return rootlessOwner
			}
			return rootlessOwner + " · " + u.Username
		}
		return rootlessOwner + " · uid:" + match[1]
	}
	return ""
}

func readContainerNames(ctx context.Context, devices []Device, names map[string]string, socket string, run func(context.Context, string, ...string) ([]byte, error)) string {
	warnings := []string{}
	read := func(rootless bool) {
		args := []string{}
		label := "Docker"
		if rootless {
			args = append(args, "--host", "unix://"+socket)
			label = rootlessOwner
		}
		args = append(args, "ps", "--no-trunc", "--format", "{{.ID}}\t{{.Names}}")
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		raw, err := run(ctx, "docker", args...)
		if err != nil {
			warnings = append(warnings, label+" 容器信息暂不可读，保留已识别归属和容器 ID")
			return
		}
		for _, line := range strings.Split(string(raw), "\n") {
			id, name, ok := strings.Cut(line, "\t")
			if !ok || len(id) != 64 || name == "" {
				continue
			}
			if names[id] == "" {
				names[id] = name
			}
			if rootless {
				for i := range devices {
					for j := range devices[i].Processes {
						p := &devices[i].Processes[j]
						if p.ContainerID == id {
							p.Owner = rootlessOwner
						}
					}
				}
			}
		}
	}
	read(false)
	// Nodes without the optional rootless service must not report an error.
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		read(true)
	}
	return strings.Join(warnings, "；")
}
