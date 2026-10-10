package rootless

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDRAMControlCannotManageRootlessEngine(t *testing.T) {
	d := &daemon{manager: newManager(), sourceSocket: "/missing/dram.sock", ctx: context.Background()}
	for _, args := range [][]string{{"restart"}, {"proxy"}, {"docker", "info"}, {"test", "up"}, {"daemon"}} {
		raw, _ := json.Marshal(controlRequest{Args: args})
		w := httptest.NewRecorder()
		d.control(w, httptest.NewRequest("POST", controlPath, strings.NewReader(string(raw))))
		if w.Code != 400 {
			t.Fatal(args, w.Code, w.Body.String())
		}
	}
	if d.source() != "/missing/dram.sock" {
		t.Fatal(d.source())
	}
	if err := d.verifySource(); err == nil {
		t.Fatal("missing sampling socket accepted")
	}
}

func TestDRAMDockerReadAPI(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "dram-api-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("unexpected mutation", r.Method)
		}
		switch r.URL.Path {
		case "/containers/json":
			if r.URL.Query().Get("all") != "1" {
				t.Error("must include stopped containers")
			}
			w.Write([]byte(`[{"Id":"abc"},{"Id":"def"}]`))
		case "/containers/abc/json":
			w.Write([]byte(`{"Id":"abc","State":{"Pid":55,"Running":true}}`))
		case "/info":
			w.Write([]byte(`{"SecurityOptions":[]}`))
		default:
			http.Error(w, "unavailable", 503)
		}
	})}
	go server.Serve(listener)
	defer server.Close()
	host := "unix://" + path
	r, err := dockerReadAPI(context.Background(), host, unixTransport(path), "container", "ls")
	if err != nil || r.Out != "abc\ndef" {
		t.Fatal(r, err)
	}
	m := newManager()
	m.dockerAPI = func(host string, args ...string) (result, error) {
		return dockerReadAPI(context.Background(), host, unixTransport(path), args...)
	}
	c, err := m.inspect(account{}, host, "--", "abc")
	if err != nil || c.Id != "abc" || c.State.Pid != 55 {
		t.Fatal(c, err)
	}
	if _, err = m.inspect(account{}, host, "missing"); err == nil {
		t.Fatal("Docker error ignored")
	}
	if _, err = dockerReadAPI(context.Background(), host, unixTransport(path), "rm", "abc"); err == nil {
		t.Fatal("mutation accepted")
	}
}

func TestServiceControlRejectsWrongManager(t *testing.T) {
	for _, source := range []string{"", dramSource} {
		d := &daemon{sourceSocket: source}
		service := "dram-bw"
		if source != "" {
			service = "rootless-docker"
		}
		raw, _ := json.Marshal(controlRequest{Args: []string{"add", "container"}, Service: service})
		w := httptest.NewRecorder()
		d.control(w, httptest.NewRequest("POST", controlPath, strings.NewReader(string(raw))))
		if w.Code != 400 || !strings.Contains(w.Body.String(), "服务不匹配") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
