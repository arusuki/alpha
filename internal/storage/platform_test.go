package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	web "project-alpha/dist"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == "worker" || os.Args[1] == "scan-helper") {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		var err error
		if os.Args[1] == "scan-helper" {
			err = ServeScanHelper(ctx, os.Stdin, os.Stdout)
		} else {
			parent, parseErr := strconv.Atoi(os.Args[4])
			err = parseErr
			if err == nil {
				err = RunWorker(ctx, os.Args[2], os.Args[3], parent)
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

func openDatabase(directory string) (*Store, error) {
	db, err := platform.OpenDatabase(directory, Initialize)
	if err != nil {
		return nil, err
	}
	return NewStore(db), nil
}

type testPlatform struct {
	t       *testing.T
	db      *Store
	m       *Manager
	s       *platform.Server
	api     *Handler
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
	m, err := NewManager(db)
	if err != nil {
		db.SQL.Close()
		t.Fatal(err)
	}
	handler := NewHandler(db, m)
	p := &testPlatform{t: t, db: db, m: m, s: platform.NewServer(db.Database, handler, web.Assets, nil, false), api: handler, storage: filepath.Join(root, "storage")}
	os.Mkdir(p.storage, 0700)
	mustWrite(t, filepath.Join(p.storage, "model.bin"), make([]byte, 8192))
	t.Cleanup(func() { m.Close(); db.SQL.Close() })
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
func (p *testPlatform) configure() Config {
	p.t.Helper()
	c := defaultConfig()
	c.NoDocker = true
	c.Root = []string{p.storage}
	p.expect(200, "PUT", "/api/settings", object{"revision": 1, "value": c}, nil)
	return c
}
func waitJob(t *testing.T, db *Store, id string) object {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		job, err := db.job(id)
		if err != nil {
			t.Fatal(err)
		}
		if !activeStatus(job["status"].(string)) {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("worker timed out")
	return nil
}
func TestAuthenticationAndGuards(t *testing.T) {
	p := newTestPlatform(t)
	if p.expect(200, "GET", "/api/session", nil, nil)["setup_required"] != true {
		t.Fatal("setup not required")
	}
	p.expect(401, "GET", "/api/state", nil, nil)
	p.expect(403, "POST", "/api/setup", nil, map[string]string{"Origin": "https://attacker.invalid"})
	p.expect(403, "GET", "/api/session", nil, map[string]string{"Host": "attacker.invalid"})
	p.login(true, "administrator", "A-test-password-123")
	p.expect(409, "POST", "/api/setup", object{"username": "administrator", "password": "A-test-password-123"}, nil)
	p.expect(403, "POST", "/api/jobs", nil, map[string]string{"X-CSRF-Token": "wrong"})
	p.expect(200, "POST", "/api/logout", nil, nil)
	p.expect(401, "GET", "/api/state", nil, nil)
	p.expect(401, "POST", "/api/login", object{"username": "administrator", "password": "wrong"}, nil)
	p.login(false, "administrator", "A-test-password-123")
	p.expect(400, "POST", "/api/password", object{"old_password": "wrong", "new_password": "New-password-456"}, nil)
	p.expect(200, "POST", "/api/password", object{"old_password": "A-test-password-123", "new_password": "New-password-456"}, nil)
	p.expect(401, "GET", "/api/state", nil, nil)
	p.login(false, "administrator", "New-password-456")
}
func TestJobsHistoryAndPersistence(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	c := p.configure()
	p.expect(409, "PUT", "/api/settings", object{"revision": 1, "value": c}, nil)
	job := p.expect(202, "POST", "/api/jobs", nil, nil)
	id := job["id"].(string)
	record := waitJob(t, p.db, id)
	if record["status"] != "completed" || record["files"] != int64(1) || record["progress"].(map[string]any)["phase"] != "completed" {
		t.Fatalf("bad completed job: %v", record)
	}
	progress := record["progress"].(map[string]any)
	if progress["capacity_known"] != true || progress["capacity_total"].(float64) <= 0 || progress["containers_remaining"] != float64(0) || progress["allocated"] != float64(record["allocated"].(int64)) {
		t.Fatalf("completed worker lost scan progress: %v", progress)
	}
	result := p.expect(200, "GET", "/api/jobs/"+id+"/snapshot", nil, nil)
	if result["tree"].(map[string]any)["files"] != float64(1) || !strings.Contains(httpapi.JSONText(result["scan"]), p.db.Directory) {
		t.Fatal("bad worker result or data exclusion")
	}
	if p.expect(200, "GET", "/api/state", nil, nil)["latest_id"] != id || p.expect(200, "GET", "/api/snapshot", nil, nil)["job_id"] != id {
		t.Fatal("latest result mismatch")
	}
	reopened, err := openDatabase(p.db.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.SQL.Close()
	settings, err := reopened.config()
	if err != nil || httpapi.JSONText(settings.Value) != httpapi.JSONText(c) {
		t.Fatal("settings did not persist")
	}
	latest, err := reopened.latest()
	if err != nil || latest == nil || *latest != id {
		t.Fatal("history did not persist")
	}
}

func TestSnapshotWorkerAssets(t *testing.T) {
	p := newTestPlatform(t)
	for _, path := range []string{"/snapshot.js", "/snapshot-loader.js", "/snapshot-worker.js", "/usage.js"} {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil)
		w := httptest.NewRecorder()
		p.s.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "worker-src 'self'") {
			t.Fatalf("snapshot worker asset unavailable: %s: %d %v", path, w.Code, w.Header())
		}
		if !strings.Contains(w.Body.String(), "use strict") {
			t.Fatalf("unexpected script body: %s", path)
		}
	}
}
func TestViewerPermissionsAndSessionRevocation(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	viewer := p.expect(201, "POST", "/api/users", object{"username": "observer", "password": "Another-password-123", "role": "viewer"}, nil)
	adminCookie, adminCSRF := p.cookie, p.csrf
	p.login(false, "observer", "Another-password-123")
	viewerCookie, viewerCSRF := p.cookie, p.csrf
	for _, route := range []string{"/api/state", "/api/jobs"} {
		p.expect(200, "GET", route, nil, nil)
	}
	for _, route := range []string{"/api/settings", "/api/users", "/api/audit"} {
		p.expect(403, "GET", route, nil, nil)
	}
	for route, method := range map[string]string{"/api/jobs": "POST", "/api/settings": "PUT", "/api/owners": "PUT", "/api/users": "POST"} {
		p.expect(403, method, route, nil, nil)
	}
	p.cookie, p.csrf = adminCookie, adminCSRF
	p.expect(200, "PATCH", "/api/users/"+viewer["id"].(string), object{"enabled": false, "role": "viewer"}, nil)
	p.cookie, p.csrf = viewerCookie, viewerCSRF
	p.expect(401, "GET", "/api/state", nil, nil)
}
func TestValidationOwnershipAuditAndAssets(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	c := p.configure()
	c.Root = []string{"relative"}
	p.expect(400, "PUT", "/api/settings", object{"revision": 2, "value": c}, nil)
	uid := p.expect(200, "GET", "/api/session", nil, nil)["user"].(map[string]any)["id"].(string)
	p.expect(409, "PATCH", "/api/users/"+uid, object{"enabled": false, "role": "admin"}, nil)
	cid := strings.Repeat("a", 64)
	p.expect(200, "PUT", "/api/owners", object{"container_id": cid, "owner": "alice"}, nil)
	p.expect(400, "PUT", "/api/owners", object{"container_id": cid, "owner": nil}, nil)
	events := p.expect(200, "GET", "/api/audit", nil, nil)
	if !strings.Contains(httpapi.JSONText(events), "container.owner") || strings.Contains(httpapi.JSONText(events), "A-test-password-123") {
		t.Fatal("invalid audit")
	}
	for _, route := range []string{"/../README.md", "/data/platform.sqlite3", "/api/exec", "/tests/fixtures/snapshot.json"} {
		p.expect(404, "GET", route, nil, nil)
	}
	for _, route := range []string{"/", "/usage.js", "/app.js", "/platform.js", "/style.css"} {
		_, _, w := p.request("GET", route, nil, nil)
		if w.Code != 200 || w.Body.Len() == 0 || w.Header().Get("Content-Security-Policy") == "" {
			t.Fatal("asset unavailable")
		}
	}
	p.expect(415, "POST", "/api/jobs", nil, map[string]string{"Content-Type": "text/plain"})
	p.expect(400, "GET", "/api/jobs?before=NaN", nil, nil)
	p.expect(403, "POST", "/api/jobs", nil, map[string]string{"Sec-Fetch-Site": "cross-site"})
}
func TestManagerCancelLockLaunchFailureAndRecovery(t *testing.T) {
	p := newTestPlatform(t)
	if other, err := NewManager(p.db); err == nil {
		other.Close()
		t.Fatal("second manager acquired lock")
	}
	p.m.mu.Lock()
	p.m.command = func(string, string) *exec.Cmd { return exec.Command("sh", "-c", "sleep 60") }
	p.m.mu.Unlock()
	job, err := p.m.Start("admin", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.m.Start("admin", "manual"); err == nil {
		t.Fatal("parallel scan accepted")
	}
	id := job["id"].(string)
	mustWrite(t, filepath.Join(p.db.Directory, "results", id, "snapshot.json"), []byte("{}"))
	cancelled, err := p.m.Cancel(id, "admin")
	if err != nil || cancelled["status"] != "cancelled" {
		t.Fatalf("cancel: %v %v", cancelled, err)
	}
	if _, err = os.Stat(filepath.Join(p.db.Directory, "results", id, "snapshot.json")); !os.IsNotExist(err) {
		t.Fatal("unpublished result retained")
	}
	p.m.Close()
	_, err = p.db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config) VALUES(?,?,?,?,?,?)", strings.Repeat("b", 32), "running", "manual", "admin", platform.Now(), httpapi.JSONText(defaultConfig()))
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(p.db)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	record, _ := p.db.job(strings.Repeat("b", 32))
	if record["status"] != "interrupted" {
		t.Fatal("unfinished job not recovered")
	}
	m.mu.Lock()
	m.command = func(string, string) *exec.Cmd { return exec.Command("/nonexistent-project-alpha-command") }
	m.mu.Unlock()
	if _, err = m.Start("admin", "manual"); err == nil {
		t.Fatal("failed launch accepted")
	}
	jobs, _ := p.db.jobs(platform.Now() + 1)
	if jobs[0]["status"] != "failed" {
		t.Fatal("launch failure left job active")
	}
}
func TestSchedulerUsesSavedConfiguration(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	c := p.configure()
	c.IntervalMinutes = 5
	p.expect(200, "PUT", "/api/settings", object{"revision": 2, "value": c}, nil)
	if err := p.m.tick(); err != nil {
		t.Fatal(err)
	}
	jobs, _ := p.db.jobs(platform.Now() + 1)
	if len(jobs) != 1 || jobs[0]["trigger"] != "scheduled" {
		t.Fatalf("schedule did not start: %v", jobs)
	}
	job := waitJob(t, p.db, jobs[0]["id"].(string))
	if job["status"] != "completed" {
		t.Fatalf("scheduled job failed: %v", job)
	}
	p.m.tick()
	jobs, _ = p.db.jobs(platform.Now() + 1)
	if len(jobs) != 1 {
		t.Fatal("schedule repeated before interval")
	}
}
func TestSnapshotFormatAndOwnerOverride(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	jid := strings.Repeat("d", 32)
	if _, err := p.db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,finished_at,config) VALUES(?,?,?,?,?,?,?)", jid, "completed", "manual", "administrator", platform.Now(), platform.Now(), httpapi.JSONText(defaultConfig())); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../tests/fixtures/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(p.db.Directory, "results", jid)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "snapshot.json"), raw)
	p.expect(200, "PUT", "/api/owners", object{"container_id": strings.Repeat("a", 64), "owner": "override"}, nil)
	snapshot := p.expect(200, "GET", "/api/snapshot", nil, nil)
	c := snapshot["containers"].([]any)[0].(map[string]any)
	if c["owner"] != "override" || c["label_owner"] != "yuuka" {
		t.Fatal("snapshot owner override failed")
	}
	var unsupported Snapshot
	if err := json.Unmarshal(raw, &unsupported); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{0, 1, 2, snapshotVersion + 1} {
		unsupported.SchemaVersion = version
		if err := atomicWrite(filepath.Join(dir, "snapshot.json"), &unsupported); err != nil {
			t.Fatal(err)
		}
		p.expect(503, "GET", "/api/snapshot", nil, nil)
	}
}
func TestConcurrentSetupAndStrictConfig(t *testing.T) {
	p := newTestPlatform(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raw := map[string]json.RawMessage{}
			json.Unmarshal([]byte(httpapi.JSONText(object{"username": fmt.Sprintf("admin%d", i), "password": "A-test-password-123"})), &raw)
			_, err := p.db.CreateUser(raw, "setup", true)
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatal("setup race created multiple admins")
	}
	raw := map[string]any{}
	json.Unmarshal([]byte(httpapi.JSONText(defaultConfig())), &raw)
	for key, value := range map[string]any{"max_depth": true, "root": nil, "no_docker": nil, "interval_minutes": 1, "unknown": 1} {
		copy := map[string]any{}
		for k, v := range raw {
			copy[k] = v
		}
		copy[key] = value
		if _, err := parseConfig([]byte(httpapi.JSONText(copy))); err == nil {
			t.Errorf("accepted invalid %s", key)
		}
	}
}
func TestLoginRateLimitAndSecureCookie(t *testing.T) {
	p := newTestPlatform(t)
	p.s.SecureCookie = true
	p.login(true, "administrator", "A-test-password-123")
	if !strings.Contains(p.cookie, "Secure") || !strings.Contains(p.cookie, "HttpOnly") || !strings.Contains(p.cookie, "SameSite=Strict") {
		t.Fatal("cookie security flags missing")
	}
	for i := 0; i < 10; i++ {
		p.expect(401, "POST", "/api/login", object{"username": "none", "password": "wrong"}, nil)
	}
	p.expect(http.StatusTooManyRequests, "POST", "/api/login", object{"username": "none", "password": "wrong"}, nil)
}

func TestWorkerCancellationStopsDockerDescendants(t *testing.T) {
	p := newTestPlatform(t)
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "child.pid")
	mustWrite(t, filepath.Join(dir, "docker"), []byte("#!/bin/sh\nsleep 60 &\necho $! > '"+pidfile+"'\nwait\n"))
	os.Chmod(filepath.Join(dir, "docker"), 0700)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	job, err := p.m.Start("admin", "manual")
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(pidfile)
		if err == nil {
			fmt.Sscan(string(raw), &pid)
			if pid > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("Docker child did not start")
	}
	cancelled, err := p.m.Cancel(job["id"].(string), "admin")
	if err != nil || cancelled["status"] != "cancelled" {
		t.Fatalf("cancel: %v %v", cancelled, err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) {
			return
		}
		if err == nil && strings.Contains(string(raw), ") Z ") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Docker descendant survived cancellation")
}
