package rootless

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"project-alpha/internal/containerorigin"
	"project-alpha/internal/procfs"
)

const auditSocket = runtimeDirectory + "/docker.sock"

var createPath = regexp.MustCompile(`^/(?:v[0-9]+\.[0-9]+/)?containers/create$`)
var dockerCgroup = regexp.MustCompile(`(?:^|/)(?:docker/|docker-)([a-f0-9]{64})(?:\.scope)?(?:/|$)`)

type sourceKey struct{}

type creationAudit struct {
	store    originStore
	bootID   string
	procRoot string
}

func (a *creationAudit) connectionContext(ctx context.Context, c net.Conn) context.Context {
	source := containerorigin.Source{Error: "无法读取连接来源"}
	if conn, ok := c.(*net.UnixConn); ok {
		if cred, err := peerCredentials(conn); err == nil {
			source = containerorigin.Source{PID: cred.Pid, UID: cred.Uid}
			a.readSource(&source)
		}
	}
	return context.WithValue(ctx, sourceKey{}, source)
}

func (a *creationAudit) readSource(source *containerorigin.Source) {
	dir := filepath.Join(a.procRoot, strconv.Itoa(int(source.PID)))
	fail := func() {
		source.ContainerID = ""
		source.Error = "连接进程已退出、身份变化或 /proc 不可读"
	}
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		fail()
		return
	}
	_, ticks, ok := procfs.ParseStat(stat)
	if !ok || ticks == 0 {
		fail()
		return
	}
	source.StartTicks = ticks
	status, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		fail()
		return
	}
	matchesUID := false
	for _, line := range strings.Split(string(status), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "Uid:" && f[2] == strconv.FormatUint(uint64(source.UID), 10) {
			matchesUID = true
		}
	}
	if !matchesUID {
		fail()
		return
	}
	cg, err := os.ReadFile(filepath.Join(dir, "cgroup"))
	if err != nil {
		fail()
		return
	}
	// Preserve only unambiguous full Docker IDs. A shared host UID is not a
	// platform user and is never used as a substitute for a source container.
	ids := map[string]bool{}
	for _, line := range strings.Split(string(cg), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}
		for _, match := range dockerCgroup.FindAllStringSubmatch(fields[2], -1) {
			ids[match[1]] = true
		}
	}
	if len(ids) > 1 {
		fail()
		return
	}
	for id := range ids {
		source.ContainerID = id
	}
	after, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		fail()
		return
	}
	_, afterTicks, ok := procfs.ParseStat(after)
	if !ok || afterTicks != ticks {
		fail()
	}
}

func (a *creationAudit) proxy(socket string) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "docker"})
	proxy.Transport = unixTransport(socket)
	proxy.FlushInterval = -1
	proxy.ModifyResponse = func(response *http.Response) error {
		r := response.Request
		if r.Method != http.MethodPost || !createPath.MatchString(path.Clean(r.URL.Path)) || response.StatusCode != http.StatusCreated {
			return nil
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		response.Body.Close()
		if err != nil {
			return err
		}
		if len(raw) > 1<<20 {
			return fmt.Errorf("Docker 创建响应过大")
		}
		response.Body = io.NopCloser(bytes.NewReader(raw))
		var result struct {
			ID string `json:"Id"`
		}
		if err = json.Unmarshal(raw, &result); err != nil {
			return fmt.Errorf("无法读取 Docker 创建结果: %w", err)
		}
		source, ok := r.Context().Value(sourceKey{}).(containerorigin.Source)
		if !ok {
			source.Error = "连接来源不可用"
		}
		record := containerorigin.Record{Version: containerorigin.Version, ContainerID: result.ID, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), BootID: a.bootID, Source: source}
		if err = a.store.save(record); err != nil {
			return fmt.Errorf("容器 %s 已创建，但来源审计保存失败；请检查审计目录，不要自动重试创建: %w", result.ID, err)
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		fmt.Fprintln(os.Stderr, "rootless Docker 代理：", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Business sockets must never expose management or audit-query endpoints.
		if strings.HasPrefix(path.Clean(r.URL.Path), "/_rootless/") {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}
