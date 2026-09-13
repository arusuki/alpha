package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func waitAgentSession(t *testing.T, p *testPlatform, id string) object {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r := p.expect(200, "GET", "/api/agent/sessions/"+id, nil, nil)
		s := r["session"].(map[string]any)
		switch s["status"] {
		case "completed", "failed", "cancelled", "interrupted":
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("agent timed out")
	return nil
}
func configureTestAgent(t *testing.T, p *testPlatform, protocol, endpoint string) {
	t.Helper()
	p.expect(200, "PUT", "/api/agent/settings", object{"revision": 1, "value": object{"protocol": protocol, "endpoint": endpoint, "model": "test-model", "api_key": "test-secret-key", "max_rounds": 4, "timeout_seconds": 10}}, nil)
	// Keep integration scans inside fixtures; production uses fullScanConfig.
	p.m.Agent.fullPlan = func(c Config) Config {
		c.Root = []string{p.storage}
		c.MaxDepth = 1
		c.MaxNodes = 100
		c.IncludeDockerRoot = false
		return c
	}
}

func TestAgentInterfacesScanToolLoopAndFollowup(t *testing.T) {
	for _, protocol := range []string{"completions", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			p := newTestPlatform(t)
			p.login(true, "administrator", "A-test-password-123")
			p.configure()
			dir := filepath.Join(p.storage, "datasets")
			os.MkdirAll(dir, 0700)
			mustWrite(t, filepath.Join(dir, "train.parquet"), make([]byte, 16384))
			var calls atomic.Int32
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body object
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				want := "/v1/responses"
				if protocol == "completions" {
					want = "/v1/chat/completions"
				}
				if r.URL.Path != want || r.Header.Get("Authorization") != "Bearer test-secret-key" {
					t.Errorf("bad provider request: %s", r.URL.Path)
				}
				var completed int
				p.db.SQL.QueryRow("SELECT count(*) FROM jobs WHERE status='completed' AND trigger='agent-full'").Scan(&completed)
				if completed != 1 {
					t.Errorf("model ran before full scan completed: %d", completed)
				}
				n := calls.Add(1)
				if n == 1 {
					if !strings.Contains(httpapi.JSONText(body), "全盘观察") || !strings.Contains(httpapi.JSONText(body), "list_containers") {
						t.Error("missing overview or tools")
					}
					arguments := httpapi.JSONText(object{"path": dir, "refresh": false})
					if protocol == "completions" {
						httpapi.WriteJSON(w, 200, object{"choices": []object{{"message": object{"role": "assistant", "content": nil, "tool_calls": []object{{"id": "call_one", "type": "function", "function": object{"name": "scan_directory", "arguments": arguments}}, {"id": "call_two", "type": "function", "function": object{"name": "scan_directory", "arguments": arguments}}}}, "finish_reason": "tool_calls"}}})
					} else {
						httpapi.WriteJSON(w, 200, object{"status": "completed", "output": []object{{"id": "rs_test", "type": "reasoning", "summary": []object{}, "encrypted_content": "opaque-reasoning"}, {"type": "function_call", "id": "fc_one", "call_id": "call_one", "name": "scan_directory", "arguments": arguments}, {"type": "function_call", "id": "fc_two", "call_id": "call_two", "name": "scan_directory", "arguments": arguments}}})
					}
					return
				}
				if n == 2 {
					encoded := httpapi.JSONText(body)
					for _, needle := range []string{"largest_files", "train.parquet", "cached", "call_one", "call_two"} {
						if !strings.Contains(encoded, needle) {
							t.Errorf("tool results missing %s", needle)
						}
					}
					if protocol == "responses" {
						if !strings.Contains(encoded, "opaque-reasoning") || !strings.Contains(encoded, "function_call_output") || body["store"] != false {
							t.Error("Responses state not replayed")
						}
					} else {
						if !strings.Contains(encoded, `"role":"tool"`) {
							t.Error("Chat tool result missing")
						}
					}
				}
				if n == 3 && !strings.Contains(httpapi.JSONText(body), "继续分析") {
					t.Error("followup missing")
				}
				if protocol == "completions" {
					httpapi.WriteJSON(w, 200, object{"choices": []object{{"message": object{"role": "assistant", "content": "统计完成：train.parquet 为数据集候选，实际分配 16384 字节。"}, "finish_reason": "stop"}}})
				} else {
					httpapi.WriteJSON(w, 200, object{"status": "completed", "output": []object{{"type": "message", "role": "assistant", "content": []object{{"type": "output_text", "text": "统计完成：train.parquet 为数据集候选，实际分配 16384 字节。"}}}}})
				}
			}))
			defer mock.Close()
			configureTestAgent(t, p, protocol, mock.URL+"/v1")
			started := p.expect(202, "POST", "/api/agent/sessions", object{"message": "先扫描再分析"}, nil)
			id := httpapi.String(started["id"])
			r := waitAgentSession(t, p, id)
			session := r["session"].(map[string]any)
			if session["status"] != "completed" {
				t.Fatalf("agent failed: %v", r)
			}
			if !strings.Contains(httpapi.JSONText(r["messages"]), "统计完成") {
				t.Fatal("final answer not persisted")
			}
			var details int
			p.db.SQL.QueryRow("SELECT count(*) FROM jobs WHERE trigger='agent-detail'").Scan(&details)
			if details != 1 {
				t.Fatalf("identical detail request scanned %d times", details)
			}
			latest, _ := p.db.latest()
			if latest == nil || *latest != session["snapshot_id"] {
				t.Fatal("detail replaced overview")
			}
			p.expect(202, "POST", "/api/agent/sessions/"+id+"/messages", object{"message": "继续分析"}, nil)
			r = waitAgentSession(t, p, id)
			if r["session"].(map[string]any)["status"] != "completed" {
				t.Fatalf("followup failed: %v", r)
			}
			var full int
			p.db.SQL.QueryRow("SELECT count(*) FROM jobs WHERE trigger='agent-full'").Scan(&full)
			if full != 1 || calls.Load() != 3 {
				t.Fatalf("followup rescanned: %d / %d", full, calls.Load())
			}
		})
	}
}

func TestAgentSettingsPermissionsAndPersistence(t *testing.T) {
	p := newTestPlatform(t)
	p.expect(401, "GET", "/api/agent/settings", nil, nil)
	p.login(true, "administrator", "A-test-password-123")
	p.expect(403, "PUT", "/api/agent/settings", object{}, map[string]string{"X-CSRF-Token": "wrong"})
	value := object{"protocol": "completions", "endpoint": "http://127.0.0.1:1234/v1", "model": "local-model", "api_key": "keep-this-private", "max_rounds": 5, "timeout_seconds": 30}
	r := p.expect(200, "PUT", "/api/agent/settings", object{"revision": 1, "value": value}, nil)
	if strings.Contains(httpapi.JSONText(r), "keep-this-private") || r["value"].(map[string]any)["has_api_key"] != true {
		t.Fatal("key leaked or was not saved")
	}
	p.expect(409, "PUT", "/api/agent/settings", object{"revision": 1, "value": value}, nil)
	p.expect(200, "PUT", "/api/agent/settings", object{"revision": 2, "value": object{"api_key": "", "model": "updated-model"}}, nil)
	c, _, _ := p.db.agentConfig()
	if c.APIKey != "keep-this-private" {
		t.Fatal("blank key erased saved key")
	}
	other, err := openDatabase(p.db.Directory)
	if err != nil {
		t.Fatal(err)
	}
	saved, _, _ := other.agentConfig()
	other.SQL.Close()
	if saved.APIKey != c.APIKey {
		t.Fatal("key not persisted")
	}
	for _, endpoint := range []string{"file:///tmp/test", "http://user:pass@example.com/v1", "https://example.com/v1?key=secret"} {
		p.expect(400, "PUT", "/api/agent/settings", object{"revision": 3, "value": object{"endpoint": endpoint}}, nil)
	}
	p.expect(200, "PUT", "/api/agent/settings", object{"revision": 3, "value": object{"clear_api_key": true}}, nil)
	c, _, _ = p.db.agentConfig()
	if c.APIKey != "" {
		t.Fatal("explicit key clear failed")
	}
	if strings.Contains(httpapi.JSONText(p.expect(200, "GET", "/api/audit", nil, nil)), "keep-this-private") {
		t.Fatal("key in audit")
	}
	p.expect(201, "POST", "/api/users", object{"username": "readonly", "password": "A-viewer-password-123", "role": "viewer"}, nil)
	p.login(false, "readonly", "A-viewer-password-123")
	p.expect(403, "GET", "/api/agent/settings", nil, nil)
	p.expect(403, "GET", "/api/agent/sessions", nil, nil)
	p.expect(403, "POST", "/api/agent/sessions", object{"message": "test"}, nil)
}

func TestAgentCancellationAndSessionIsolation(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	p.configure()
	entered := make(chan struct{})
	disconnected := make(chan struct{})
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
		close(disconnected)
	}))
	defer func() { p.m.Agent.Close(); mock.Close() }()
	configureTestAgent(t, p, "responses", mock.URL)
	start := p.expect(202, "POST", "/api/agent/sessions", object{"message": "test cancellation"}, nil)
	id := httpapi.String(start["id"])
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("model did not start")
	}
	p.expect(409, "POST", "/api/agent/sessions", object{"message": "concurrent"}, nil)
	p.expect(201, "POST", "/api/users", object{"username": "secondadmin", "password": "Second-admin-pass-123", "role": "admin"}, nil)
	oldCookie, oldCSRF := p.cookie, p.csrf
	p.login(false, "secondadmin", "Second-admin-pass-123")
	p.expect(404, "GET", "/api/agent/sessions/"+id, nil, nil)
	p.expect(404, "POST", "/api/agent/sessions/"+id+"/cancel", object{}, nil)
	p.cookie, p.csrf = oldCookie, oldCSRF
	p.expect(200, "POST", "/api/agent/sessions/"+id+"/cancel", object{}, nil)
	r := waitAgentSession(t, p, id)
	if r["session"].(map[string]any)["status"] != "cancelled" {
		t.Fatalf("not cancelled: %v", r)
	}
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("model HTTP request was not cancelled")
	}
}

func TestAgentProviderErrorsAndEndpoint(t *testing.T) {
	for _, protocol := range []string{"responses", "completions"} {
		for _, endpoint := range []string{"https://example.com", "https://example.com/v1/", "https://example.com/v1/chat/completions", "https://example.com/v1/responses"} {
			c := defaultAgentConfig()
			c.Protocol = protocol
			c.Endpoint = endpoint
			got, err := c.endpointURL()
			want := "https://example.com/v1/responses"
			if protocol == "completions" {
				want = "https://example.com/v1/chat/completions"
			}
			if err != nil || got != want {
				t.Fatalf("endpoint: %s %v", got, err)
			}
		}
	}
	for _, test := range []struct {
		status int
		body   string
	}{{401, `{"error":"secret-key-value"}`}, {200, `{"choices":[]}`}, {200, `{"choices":[{"message":null}]}`}, {200, `{"choices":[{"message":{"content":"partial"},"finish_reason":"length"}]}`}, {200, `{"choices":[{"message":{"tool_calls":[{"id":"a","type":"function","function":{"name":"x","arguments":"{}"}},{"id":"a","type":"function","function":{"name":"x","arguments":"{}"}}]}}]}`}} {
		mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.status); fmt.Fprint(w, test.body) }))
		c := defaultAgentConfig()
		c.Protocol = "completions"
		c.Endpoint = mock.URL
		c.APIKey = "secret-key-value"
		_, err := (agentProvider{Config: c}).complete(context.Background(), []object{}, true)
		mock.Close()
		if err == nil || strings.Contains(err.Error(), c.APIKey) {
			t.Fatalf("bad error handling: %v", err)
		}
	}
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Store(true) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	c := defaultAgentConfig()
	c.Endpoint = redirect.URL
	c.APIKey = "secret"
	_, err := (agentProvider{Config: c}).complete(context.Background(), nil, true)
	if err == nil || redirected.Load() {
		t.Fatal("followed model redirect with credentials")
	}
}

func TestDetailScanMetadataAndGuard(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "old.parquet")
	mustWrite(t, file, make([]byte, 8192))
	old := time.Now().Add(-200 * 24 * time.Hour)
	os.Chtimes(file, old, old)
	os.Link(file, filepath.Join(dir, "alias.parquet"))
	mustWrite(t, filepath.Join(dir, "new.jsonl"), make([]byte, 16384))
	c := defaultConfig()
	c.NoDocker = true
	c.Root = []string{dir}
	c.MaxDepth = 0
	c.MaxNodes = 100
	s, err := buildDetailSnapshot(context.Background(), c, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Analysis == nil || len(s.Analysis.Largest) != 2 || s.Analysis.Modified[2].Files != 1 || s.Analysis.Largest[0].Apparent != 16384 {
		t.Fatalf("incorrect metadata: %+v", s.Analysis)
	}
	if len(s.Analysis.Types) != 2 || s.Analysis.Modified[0].Files != 1 {
		t.Fatal("hardlink counted twice or age wrong")
	}
	c.Exclude = []string{filepath.Join(dir, "excluded")}
	os.MkdirAll(c.Exclude[0], 0700)
	os.Symlink(c.Exclude[0], filepath.Join(dir, "escape"))
	for _, path := range []string{"relative", "/proc/1", "/sys", "/dev", "/run", "/var/lib/docker/overlay2/layer/merged", filepath.Join(dir, "escape")} {
		if _, err := validateDetailPath(path, c, ""); err == nil {
			t.Fatalf("accepted forbidden path %s", path)
		}
	}
	c = fullScanConfig(defaultConfig())
	if !c.IncludeDockerRoot || !containsString(c.Root, "/") {
		t.Fatal("production baseline is not full disk")
	}
}
func containsString(a []string, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}

func TestAgentUsageMatchesFrontend(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	raw, err := os.ReadFile("../../tests/fixtures/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	if err = json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	u := buildUsage(&s)
	code := `const U=require('./dist/usage.js'),s=require('./tests/fixtures/snapshot.json'),u=U.build(s);console.log(JSON.stringify({exclusive:u.exclusive,shared:u.shared,crossOwner:u.crossOwner,unrelated:u.unrelated,containers:[...u.containers].map(([id,r])=>({id,exclusive:r.exclusive,shared:r.shared,known:r.known,partial:r.partial}))}));`
	command := exec.Command("node", "-e", code)
	command.Dir = "../.."
	out, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var js struct {
		Exclusive, Shared, CrossOwner, Unrelated int64
		Containers                               []struct {
			ID                string
			Exclusive, Shared int64
			Known, Partial    bool
		}
	}
	if err = json.Unmarshal(out, &js); err != nil {
		t.Fatal(err)
	}
	if u.Exclusive != js.Exclusive || u.Shared != js.Shared || u.CrossOwner != js.CrossOwner || u.Unrelated != js.Unrelated {
		t.Fatalf("accounting differs: %+v / %s", u, out)
	}
	for _, expected := range js.Containers {
		r := u.Containers[expected.ID]
		if r == nil || r.Exclusive != expected.Exclusive || r.Shared != expected.Shared || r.Known != expected.Known || r.Partial != expected.Partial {
			t.Fatalf("container differs: %+v / %+v", r, expected)
		}
	}
}

func TestAgentRecovery(t *testing.T) {
	db, err := openDatabase(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	_, err = db.SQL.Exec("INSERT INTO users(id,username,password_hash,role,enabled,created_at) VALUES('owner','owner','unused','admin',1,?)", platform.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.SQL.Exec("INSERT INTO agent_sessions(id,user_id,title,status,created_at,updated_at,snapshot_id,active_job_id,provider,model) VALUES('session','owner','previous analysis','running',?,?, 'baseline','old-job','responses','test')", platform.Now(), platform.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.SQL.Exec("INSERT INTO agent_messages(session_id,role,content,created_at) VALUES('session','assistant','saved result',?)", platform.Now())
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(db)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s, err := m.Agent.session("session", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if s["status"] != "interrupted" || s["snapshot_id"] != "baseline" || s["active_job_id"] != nil {
		t.Fatalf("bad recovery: %v", s)
	}
	var text string
	db.SQL.QueryRow("SELECT content FROM agent_messages WHERE session_id='session'").Scan(&text)
	if text != "saved result" {
		t.Fatal("recovery lost messages")
	}
}

func TestAgentFullScanFailureNeverCallsModel(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	p.configure()
	var called atomic.Bool
	mock := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Store(true) }))
	defer mock.Close()
	configureTestAgent(t, p, "responses", mock.URL)
	p.m.command = func(string, string) *exec.Cmd { return exec.Command("false") }
	s := p.expect(202, "POST", "/api/agent/sessions", object{"message": "scan first"}, nil)
	r := waitAgentSession(t, p, httpapi.String(s["id"]))
	session := r["session"].(map[string]any)
	if session["status"] != "failed" || session["snapshot_id"] != nil || called.Load() {
		t.Fatalf("failed scan reached model: %v", r)
	}
}

func TestAgentToolsRejectInvalidCalls(t *testing.T) {
	tools := &agentTools{}
	for _, c := range []struct{ name, args string }{{"shell", `{"command":"du /"}`}, {"get_overview", `{"unexpected":true}`}, {"scan_directory", `{"path":"/tmp"}`}, {"get_directory", `{"path":"/tmp","offset":-1,"limit":30}`}, {"list_containers", `{"query":"","sort_by":"exclusive","offset":0,"limit":1000}`}, {"scan_directory", `null`}} {
		if _, err := tools.call(context.Background(), c.name, c.args); err == nil {
			t.Fatalf("accepted invalid call: %+v", c)
		}
	}
}

func TestBoundedJSONPreservesUTF8AndByteBudget(t *testing.T) {
	for _, text := range []string{strings.Repeat("目录", 9000), strings.Repeat("📁", 13000), strings.Repeat("x", 50000)} {
		var result struct {
			Truncated bool
			Preview   string
		}
		encoded := boundedJSON(object{"path": text})
		if err := json.Unmarshal([]byte(encoded), &result); err != nil {
			t.Fatal(err)
		}
		if !result.Truncated || len(result.Preview) > 45000 || len(result.Preview) < 44997 || strings.ContainsRune(result.Preview, '\uFFFD') || !strings.HasPrefix(httpapi.JSONText(object{"path": text}), result.Preview) {
			t.Fatalf("invalid UTF-8 preview: truncated=%v bytes=%d", result.Truncated, len(result.Preview))
		}
	}
	small := object{"path": "/目录/📁"}
	if got := boundedJSON(small); got != httpapi.JSONText(small) {
		t.Fatalf("small result changed: %s", got)
	}
}

func TestAgentMessagePaginationBoundary(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	var uid string
	if err := p.db.SQL.QueryRow("SELECT id FROM users").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	id := platform.RandomHex(16)
	if _, err := p.db.SQL.Exec("INSERT INTO agent_sessions(id,user_id,title,status,created_at,updated_at,provider,model) VALUES(?,?,?,'completed',?,?,'completions','test')", id, uid, "pagination", platform.Now(), platform.Now()); err != nil {
		t.Fatal(err)
	}
	for count := 0; count <= 201; count++ {
		if count > 0 {
			if err := p.m.Agent.message(id, "assistant", fmt.Sprint(count), ""); err != nil {
				t.Fatal(err)
			}
		}
		if count != 0 && count != 199 && count != 200 && count != 201 {
			continue
		}
		page := p.expect(200, "GET", "/api/agent/sessions/"+id, nil, nil)
		messages := page["messages"].([]any)
		if len(messages) != min(count, 200) || page["has_more"] != (count > 200) {
			t.Fatalf("count=%d page=%v", count, page)
		}
		if count == 201 {
			next := p.expect(200, "GET", fmt.Sprintf("/api/agent/sessions/%s?after=%d", id, numberInt64(page["next_after"])), nil, nil)
			tail := next["messages"].([]any)
			if len(tail) != 1 || tail[0].(map[string]any)["content"] != "201" || next["has_more"] != false {
				t.Fatalf("bad last page: %v", next)
			}
		}
	}
}

func TestAgentRetriesFailedCompletionWithoutClientPolling(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	p.configure()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fail only the terminal write, after scans and messages have succeeded.
		if _, err := p.db.SQL.Exec("CREATE TRIGGER fail_agent_completion BEFORE UPDATE OF status ON agent_sessions WHEN NEW.status='completed' BEGIN SELECT RAISE(FAIL,'injected completion failure'); END"); err != nil {
			t.Error(err)
		}
		httpapi.WriteJSON(w, 200, object{"choices": []object{{"message": object{"role": "assistant", "content": "扫描完成"}}}})
	}))
	defer mock.Close()
	configureTestAgent(t, p, "completions", mock.URL)
	session := p.expect(202, "POST", "/api/agent/sessions", object{"message": "分析磁盘"}, nil)
	id := session["id"].(string)
	a := p.m.Agent
	deadline := time.Now().Add(20 * time.Second)
	pending := false
	for time.Now().Before(deadline) {
		a.mu.Lock()
		pending = a.pending != nil
		if pending && (a.active != id || a.pending.status != "completed") {
			t.Errorf("lost pending completion: active=%q pending=%+v", a.active, a.pending)
		}
		a.mu.Unlock()
		if pending {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !pending {
		t.Fatal("completion failure was not retained")
	}
	if _, err := a.start(id, session["user_id"].(string), "administrator", "继续"); err == nil {
		t.Fatal("allowed followup before completion persisted")
	}
	if _, err := p.db.SQL.Exec("DROP TRIGGER fail_agent_completion"); err != nil {
		t.Fatal(err)
	}
	// Observe storage directly: no API request or explicit retry should be needed.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := p.db.SQL.QueryRow("SELECT status FROM agent_sessions WHERE id=?", id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "completed" {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.active != "" || a.pending != nil || a.cancel != nil {
				t.Fatal("persisted completion did not release ownership")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("background retry did not recover")
}
