package containers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

type command func(context.Context, string, []string, string) (string, error)

func runDocker(ctx context.Context, endpoint string, args []string, input string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", append([]string{"--host", endpoint}, args...)...)
	cmd.Env = slices.DeleteFunc(os.Environ(), func(s string) bool {
		return strings.HasPrefix(s, "DOCKER_HOST=") || strings.HasPrefix(s, "DOCKER_CONTEXT=")
	})
	cmd.Stdin = strings.NewReader(input)
	cmd.WaitDelay = time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("Docker %s 超时或取消；请刷新状态确认结果: %w", args[0], ctx.Err())
	}
	if err != nil {
		// Password-bearing exec failures must never echo stdin back through the API.
		if input != "" {
			return "", fmt.Errorf("Docker %s 失败（密码未回显）: %w", args[0], err)
		}
		return "", fmt.Errorf("Docker %s 失败: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

var fullID = regexp.MustCompile(`^[a-f0-9]{64}$`)
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

type mount struct {
	Type, Source, Destination string
	RW                        bool
}
type binding struct{ HostIP, HostPort string }
type device struct {
	Driver       string
	Count        int
	DeviceIDs    []string
	Capabilities [][]string
}
type inspection struct {
	ID     string `json:"Id"`
	Name   string
	Image  string
	Config struct {
		Image, Hostname, User string
		Tty                   bool
		Cmd, Entrypoint, Env  []string
		Labels                map[string]string
	}
	HostConfig struct {
		NetworkMode, IpcMode, PidMode          string
		Privileged, ReadonlyRootfs, AutoRemove bool
		RestartPolicy                          struct{ Name string }
		Ulimits                                []struct {
			Name       string
			Soft, Hard int64
		}
		DeviceRequests []device
		PortBindings   map[string][]binding
	}
	State struct {
		Status                            string
		Running, Paused, Restarting, Dead bool
	}
	Mounts []mount
	raw    map[string]json.RawMessage
}

func (h *Handler) daemon(ctx context.Context, endpoint string) (string, error) {
	out, err := h.run(ctx, endpoint, []string{"info", "--format", "{{json .}}"}, "")
	if err != nil {
		return "", err
	}
	var info struct{ ID, OSType string }
	if json.Unmarshal([]byte(out), &info) != nil || info.ID == "" || info.OSType != "linux" {
		return "", fmt.Errorf("无法确认 Linux Docker daemon 身份")
	}
	return info.ID, nil
}
func (h *Handler) inspect(ctx context.Context, endpoint, ref string) (inspection, error) {
	var c inspection
	if !validName.MatchString(ref) && !fullID.MatchString(ref) {
		return c, fmt.Errorf("容器名或 ID 无效")
	}
	out, err := h.run(ctx, endpoint, []string{"container", "inspect", ref}, "")
	if err != nil {
		return c, err
	}
	var rows []json.RawMessage
	if json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 {
		return c, fmt.Errorf("Docker inspect 未返回唯一容器")
	}
	if err = json.Unmarshal(rows[0], &c); err != nil {
		return c, fmt.Errorf("Docker inspect 格式无效: %w", err)
	}
	if !fullID.MatchString(c.ID) {
		return c, fmt.Errorf("Docker 未返回完整容器 ID")
	}
	if (fullID.MatchString(ref) && ref != c.ID) || (!fullID.MatchString(ref) && strings.TrimPrefix(c.Name, "/") != ref) {
		return c, fmt.Errorf("Docker 返回的容器与请求不一致")
	}
	if err = json.Unmarshal(rows[0], &c.raw); err != nil {
		return c, err
	}
	return c, nil
}
func fingerprint(c inspection) string {
	// Hash immutable config (including old embedded passwords) without storing or
	// returning it. Runtime state, network addresses and log paths are excluded.
	values := map[string]any{"Id": c.ID, "Name": c.Name, "Image": c.Image, "Config": c.raw["Config"], "HostConfig": c.raw["HostConfig"], "Mounts": c.raw["Mounts"]}
	raw, _ := json.Marshal(values)
	var canonical map[string]any
	_ = json.Unmarshal(raw, &canonical)
	// Mount ordering in inspect has no semantic meaning and may change across
	// daemon restarts. Preserve every mount field while normalizing its order.
	if mounts, ok := canonical["Mounts"].([]any); ok {
		slices.SortFunc(mounts, func(a, b any) int {
			left, _ := a.(map[string]any)
			right, _ := b.(map[string]any)
			ld, _ := left["Destination"].(string)
			rd, _ := right["Destination"].(string)
			return strings.Compare(ld, rd)
		})
	}
	raw, _ = json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
