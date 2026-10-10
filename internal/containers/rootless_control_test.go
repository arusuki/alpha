package containers

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestRootlessUnsupportedCommand(t *testing.T) {
	s := newServiceFixture(t)
	if code, out := s.configure(t, "alice"); code != 200 {
		t.Fatal(code, out)
	}
	socket := t.TempDir() + "/control.sock"
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Args []string }
		if r.Method != "POST" || r.URL.Path != "/_rootless/control" || json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Args) != 1 || request.Args[0] != "bindings" {
			t.Error("unexpected control request")
			http.Error(w, "unexpected request", 500)
			return
		}
		http.Error(w, "未知命令：bindings", http.StatusBadRequest)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	s.handler.rootlessControl = func(ctx context.Context, _ string, args []string) (string, error) {
		return callRootless(ctx, socket, args)
	}
	code, out := call(s.handler, "GET", servicesPath, "", admin)
	if code != 200 || !strings.Contains(out, "重新构建并部署 rootless-docker") || !strings.Contains(out, socket) || !strings.Contains(out, "未知命令：bindings") || strings.Contains(out, `"state":"mounted"`) {
		t.Fatal(code, out)
	}
	// Failed verification must never revoke access or discard the journal.
	if code, out = s.configure(t); code == 200 || !strings.Contains(out, "重新构建并部署") {
		t.Fatal(code, out)
	}
	c, err := s.handler.serviceConfig(rootlessService)
	if err != nil || len(c.Users) != 1 || c.Users[0] != "alice" {
		t.Fatal(c, err)
	}
	mounts, err := s.handler.serviceMounts()
	if err != nil || len(mounts) != 1 || mounts[0].Owner != "alice" || s.handler.checkServiceMounts(s.docker.c.ID) == nil {
		t.Fatal("lost mount protection", mounts, err)
	}
}

func TestRootlessOtherControlError(t *testing.T) {
	socket := t.TempDir() + "/control.sock"
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "请求格式无效", http.StatusBadRequest)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	_, err = callRootless(context.Background(), socket, []string{"bindings"})
	if err == nil || err.Error() != "Rootless 管理请求失败（400）：请求格式无效" {
		t.Fatal("unrelated error misreported as unsupported command", err)
	}
}
