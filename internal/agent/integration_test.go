package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	web "project-alpha/dist"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
	"project-alpha/internal/storage"
)

type testModules struct{ storage, agent platform.Module }

func (m testModules) Dispatch(w http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
	if IsRoute(r.URL.Path) {
		return m.agent.Dispatch(w, r, u)
	}
	status, value, err := m.storage.Dispatch(w, r, u)
	if err == nil && r.Method == "DELETE" && status == 200 {
		if result, ok := value.(map[string]any); ok {
			if ids, ok := result["deleted_ids"].([]string); ok {
				err = m.agent.(*Handler).DB.ForgetRecords(ids)
			}
		}
	}
	return status, value, err
}

type testRecords struct {
	*storage.Service
	failStart bool
	cleanup   func(context.Context, string, string, []string, []byte, func(string, string, string) error) error
}

func (r *testRecords) StartOverview(actor string) (object, error) {
	if r.failStart {
		return nil, fmt.Errorf("injected scan failure")
	}
	return r.Manager.Start(actor, "agent-full")
}
func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == "worker" || os.Args[1] == "scan-helper") {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		var err error
		if os.Args[1] == "scan-helper" {
			err = storage.ServeScanHelper(ctx, os.Stdin, os.Stdout)
		} else {
			parent, parseErr := strconv.Atoi(os.Args[4])
			err = parseErr
			if err == nil {
				err = storage.RunWorker(ctx, os.Args[2], os.Args[3], parent)
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func openDatabase(directory string) (*platform.Database, error) {
	return platform.OpenDatabase(directory, func(tx *sql.Tx) error {
		if err := storage.Initialize(tx); err != nil {
			return err
		}
		return Initialize(tx)
	})
}

type testPlatform struct {
	t       *testing.T
	db      *platform.Database
	m       *storage.Manager
	agent   *Manager
	records *testRecords
	s       *platform.Server
	api     *storage.Handler
	cookie  string
	csrf    string
	storage string
}

func newTestPlatform(t *testing.T) *testPlatform {
	t.Helper()
	root := t.TempDir()
	db, err := openDatabase(filepath.Join(root, "platform"))
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewStore(db)
	m, err := storage.NewManager(store)
	if err != nil {
		db.SQL.Close()
		t.Fatal(err)
	}
	handler := storage.NewHandler(store, m)
	records := &testRecords{Service: handler.Service}
	am, err := NewManager(NewStore(db), records, testAuthorization(db))
	if err != nil {
		m.Close()
		db.SQL.Close()
		t.Fatal(err)
	}
	modules := testModules{handler, NewHandler(am.db, am)}
	p := &testPlatform{t: t, db: db, m: m, agent: am, records: records, s: platform.NewServer(db, modules, web.Assets, nil, false), api: handler, storage: filepath.Join(root, "storage")}
	os.Mkdir(p.storage, 0700)
	mustWrite(t, filepath.Join(p.storage, "model.bin"), make([]byte, 8192))
	t.Cleanup(func() { am.Close(); m.Close(); db.SQL.Close() })
	return p
}
func (p *testPlatform) request(method, path string, value any, headers map[string]string) (int, object, *httptest.ResponseRecorder) {
	p.t.Helper()
	raw := []byte("{}")
	if value != nil {
		raw = []byte(httpapi.JSONText(value))
	}
	r := httptest.NewRequest(method, "http://127.0.0.1"+path, bytes.NewReader(raw))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://127.0.0.1")
	r.Header.Set("X-CSRF-Token", p.csrf)
	if p.cookie != "" {
		r.Header.Set("Cookie", p.cookie)
	}
	for k, v := range headers {
		if k == "Host" {
			r.Host = v
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	p.s.ServeHTTP(w, r)
	var out object
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, w
}
func (p *testPlatform) expect(status int, method, path string, value any, headers map[string]string) object {
	p.t.Helper()
	actual, out, _ := p.request(method, path, value, headers)
	if actual != status {
		p.t.Fatalf("%s %s: want %d got %d: %v", method, path, status, actual, out)
	}
	return out
}
func (p *testPlatform) login(setup bool, name, password string) {
	p.t.Helper()
	route := "/api/login"
	if setup {
		route = "/api/setup"
	}
	status, out, w := p.request("POST", route, object{"username": name, "password": password}, nil)
	if status != 200 {
		p.t.Fatalf("login: %d %v", status, out)
	}
	p.csrf = out["csrf"].(string)
	p.cookie = w.Result().Cookies()[0].String()
}
func (p *testPlatform) configure() storage.Config {
	p.t.Helper()
	var c storage.Config
	current := p.expect(200, "GET", "/api/settings", nil, nil)
	if err := json.Unmarshal([]byte(httpapi.JSONText(current["value"])), &c); err != nil {
		p.t.Fatal(err)
	}
	c.NoDocker = true
	c.Root = []string{p.storage}
	c.MaxDepth = 1
	c.MaxNodes = 100
	c.IncludeDockerRoot = false
	p.expect(200, "PUT", "/api/settings", object{"revision": 1, "value": c}, nil)
	return c
}
func waitJob(t *testing.T, db *testRecords, id string) object {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		job, err := db.Job(id)
		if err != nil {
			t.Fatal(err)
		}
		if !activeJobStatus(job["status"].(string)) {
			if job["status"] == "completed" {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := db.Wait(ctx, id, nil); err != nil {
					t.Fatal(err)
				}
			}
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("worker timed out")
	return nil
}

func activeJobStatus(s string) bool { return s == "queued" || s == "running" || s == "cancelling" }

// Mirror central authorization for the in-process test server.
func testAuthorization(db *platform.Database) func(string) error {
	return func(id string) error {
		var role string
		var enabled bool
		err := db.SQL.QueryRow("SELECT role,enabled FROM users WHERE id=?", id).Scan(&role, &enabled)
		if err != nil || role != "admin" || !enabled {
			return httpapi.NewError(403, "分析发起人的管理员权限已失效")
		}
		return nil
	}
}
