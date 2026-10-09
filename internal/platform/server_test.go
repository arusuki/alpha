package platform

import (
	"bytes"
	"mime"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	web "project-alpha/dist"
	"project-alpha/internal/testutil"
)

func TestEmbeddedBrowserAssets(t *testing.T) {
	db, err := OpenDatabase(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	server := NewServer(db, nil, web.Assets, nil, false)

	entries, err := web.Assets.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Run(entry.Name(), func(t *testing.T) {
			path := "/" + entry.Name()
			if entry.Name() == "index.html" {
				path = "/"
			}
			if entry.Name() == "status.html" {
				path = "/status/alice"
			}
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s: status %d, body %s", path, response.Code, response.Body.String())
			}
			if !strings.Contains(response.Header().Get("Content-Security-Policy"), "worker-src 'self'") {
				t.Fatal("browser worker blocked by CSP")
			}
			if got, want := response.Header().Get("Content-Type"), mime.TypeByExtension(filepath.Ext(entry.Name())); got != want {
				t.Fatalf("GET %s: Content-Type %q, want %q", path, got, want)
			}
			body, err := web.Assets.ReadFile(entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(response.Body.Bytes(), body) {
				t.Fatalf("GET %s: response differs from embedded asset", path)
			}
		})
	}
}

func TestDirectControlAccessUsesAllowedHosts(t *testing.T) {
	db, err := OpenDatabase(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	client := &testutil.Client{T: t, Handler: NewServer(db, nil, nil, []string{"10.0.0.1", "fd7a:115c:a1e0::1", "control.example"}, false)}
	for _, host := range []string{"127.0.0.1:8765", "10.0.0.1:8765", "[FD7A:115C:A1E0::1]:8443", "control.example"} {
		client.Expect(200, "GET", "/api/session", nil, map[string]string{"Host": host})
	}
	for _, host := range []string{"10.0.0.2:8765", "[fd7a:115c:a1e0::2]:8765", "other.example"} {
		client.Expect(403, "GET", "/api/session", nil, map[string]string{"Host": host})
	}
}

func TestShareEntranceBlocksManagementEvenWithAllowedHostAndAdminSession(t *testing.T) {
	db, err := OpenDatabase(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	if _, err = db.SQL.Exec("CREATE TABLE bastion_tailscale(id TEXT,name TEXT,enabled INTEGER,ssh_host TEXT,ssh_port INTEGER,status_port INTEGER,control_url TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SQL.Exec("INSERT INTO bastion_tailscale VALUES('share','Share',1,'100.64.0.2',22,9765,'http://10.0.0.1:8765')"); err != nil {
		t.Fatal(err)
	}
	server := NewServer(db, nil, web.Assets, []string{"100.64.0.2"}, false)
	client := &testutil.Client{T: t, Handler: server}
	client.Login(true, "admin", "password-123456")
	for _, path := range []string{"/", "/nodes/" + strings.Repeat("a", 32) + "/", "/app.js", "/api/session", "/api/cluster/overview", "/api/users"} {
		client.Expect(403, "GET", path, nil, map[string]string{"Host": "100.64.0.2:9765"})
	}
	for _, path := range []string{"/api/setup", "/api/login"} {
		client.Expect(403, "POST", path, map[string]string{}, map[string]string{"Host": "100.64.0.2:9765"})
	}
	for _, path := range []string{"/status/alice", "/status.js", "/status.css", "/gpu.js", "/gpu.css", "/member-disk.js", "/usage.js", "/clipboard.js"} {
		client.Expect(200, "GET", path, nil, map[string]string{"Host": "100.64.0.2:9765"})
	}
	client.Expect(200, "GET", "/", nil, nil)
}
