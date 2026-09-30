package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	web "project-alpha/dist"
	"project-alpha/internal/credentials"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

const testToken = "tskey-api-private-test-token"

type transportFunc func(*http.Request) (*http.Response, error)

func (fn transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

type harness struct {
	h      *Handler
	server *platform.Server
	cookie string
	csrf   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := platform.OpenDatabase(t.TempDir(), Initialize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	h := NewHandler(db)
	h.client.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		t.Error("unexpected upstream call")
		return response(500, ""), nil
	})
	return &harness{h: h, server: platform.NewServer(db, h, web.Assets, nil, false)}
}
func (p *harness) request(t *testing.T, method, path, body string, status int) map[string]any {
	t.Helper()
	r := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if p.cookie != "" {
		r.Header.Set("Cookie", p.cookie)
	}
	r.Header.Set("X-CSRF-Token", p.csrf)
	w := httptest.NewRecorder()
	p.server.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: got %d, want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	if strings.Contains(w.Body.String(), testToken) {
		t.Fatal("credential leaked in response")
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(path, "/api/setup") || path == "/api/login" {
		if status == 200 || status == 201 {
			p.cookie = w.Result().Cookies()[0].String()
			p.csrf = result["csrf"].(string)
		}
	}
	return result
}
func (p *harness) login(t *testing.T) {
	t.Helper()
	p.request(t, "POST", "/api/setup", `{"username":"administrator","password":"A-test-password-123"}`, 200)
}
func fields(raw string) map[string]json.RawMessage {
	var value map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		panic(err)
	}
	return value
}
func checkError(t *testing.T, err error, status int) {
	t.Helper()
	var api *httpapi.Error
	if !errors.As(err, &api) || api.Status != status {
		t.Fatalf("expected %d, got %v", status, err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatal("credential leaked in error")
	}
}
func TestSettingsEncryptionPersistenceAndPermissions(t *testing.T) {
	p := newHarness(t)
	for _, endpoint := range []struct{ method, path, body string }{{"GET", "settings", ""}, {"PUT", "settings", "{}"}, {"GET", "devices", ""}, {"POST", "test", "{}"}} {
		p.request(t, endpoint.method, "/api/tailscale/"+endpoint.path, endpoint.body, 401)
	}
	p.login(t)
	p.request(t, "GET", "/api/tailscale/devices", "", 409)
	p.request(t, "POST", "/api/tailscale/test", "{}", 409)
	savedCSRF := p.csrf
	p.csrf = "wrong"
	p.request(t, "PUT", "/api/tailscale/settings", "{}", 403)
	p.request(t, "POST", "/api/tailscale/test", "{}", 403)
	p.csrf = savedCSRF
	body := `{"revision":1,"tailnet":"-","api_token":"` + testToken + `"}`
	s := p.request(t, "PUT", "/api/tailscale/settings", body, 200)
	if s["has_api_token"] != true || s["revision"] != float64(2) {
		t.Fatal(s)
	}
	_, encrypted, err := p.h.settings()
	if err != nil || !strings.HasPrefix(encrypted, credentials.Prefix) || strings.Contains(encrypted, testToken) {
		t.Fatalf("not encrypted: %v", err)
	}
	keypath := filepath.Join(p.h.db.Directory, tokenKeyFile)
	info, err := os.Stat(keypath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("key permissions: %v", err)
	}
	p.request(t, "PUT", "/api/tailscale/settings", body, 409)
	p.request(t, "PUT", "/api/tailscale/settings", `{"revision":2,"tailnet":"T123CNTRL","api_token":""}`, 200)
	db, err := platform.OpenDatabase(p.h.db.Directory, Initialize)
	if err != nil {
		t.Fatal(err)
	}
	reopened := NewHandler(db)
	settings, ciphertext, err := reopened.settings()
	if err != nil || settings.Tailnet != "T123CNTRL" || settings.Revision != 3 || ciphertext != encrypted {
		t.Fatalf("persistence: %+v %v", settings, err)
	}
	token, err := reopened.decrypt(ciphertext)
	db.SQL.Close()
	if err != nil || token != testToken {
		t.Fatal("credential not preserved")
	}
	audit := p.request(t, "GET", "/api/audit", "", 200)
	if strings.Contains(httpapi.JSONText(audit), testToken) {
		t.Fatal("credential leaked in audit")
	}
	p.request(t, "PUT", "/api/tailscale/settings", `{"revision":3,"tailnet":"-","api_token":"tskey-api-replacement"}`, 200)
	_, ciphertext, _ = p.h.settings()
	token, err = p.h.decrypt(ciphertext)
	if err != nil || token != "tskey-api-replacement" {
		t.Fatal("replacement failed")
	}
	s = p.request(t, "PUT", "/api/tailscale/settings", `{"revision":4,"tailnet":"-","clear_api_token":true}`, 200)
	if s["has_api_token"] != false {
		t.Fatal("clear failed")
	}
	_, ciphertext, _ = p.h.settings()
	if ciphertext != "" {
		t.Fatal("ciphertext not cleared")
	}
	p.request(t, "GET", "/api/tailscale/devices", "", 409)
	p.request(t, "POST", "/api/users", `{"username":"readonly","password":"A-viewer-password-123","role":"viewer"}`, 201)
	p.request(t, "POST", "/api/login", `{"username":"readonly","password":"A-viewer-password-123"}`, 200)
	for _, endpoint := range []struct{ method, path, body string }{{"GET", "settings", ""}, {"PUT", "settings", "{}"}, {"GET", "devices", ""}, {"POST", "test", "{}"}} {
		p.request(t, endpoint.method, "/api/tailscale/"+endpoint.path, endpoint.body, 403)
	}
}

func TestSettingsValidationAndConcurrentWrites(t *testing.T) {
	p := newHarness(t)
	p.login(t)
	for _, body := range []string{
		`{}`, `{"revision":1,"tailnet":null}`, `{"revision":"1","tailnet":"-"}`,
		`{"revision":1,"tailnet":"https://example.com"}`, `{"revision":1,"tailnet":"../other"}`,
		`{"revision":1,"tailnet":"a/b"}`, `{"revision":1,"tailnet":"-","api_token":null}`,
		`{"revision":1,"tailnet":"-","api_token":"tskey-client-secret"}`,
		`{"revision":1,"tailnet":"-","api_token":"tskey-auth-secret"}`,
		`{"revision":1,"tailnet":"-","api_token":"tskey-api-a\nb"}`,
		`{"revision":1,"tailnet":"-","clear_api_token":"yes"}`,
		`{"revision":1,"tailnet":"-","api_token":"tskey-api-new","clear_api_token":true}`,
		`{"revision":1,"tailnet":"-","endpoint":"https://attacker.invalid"}`,
	} {
		p.request(t, "PUT", "/api/tailscale/settings", body, 400)
	}
	p.request(t, "POST", "/api/tailscale/test", `{"api_token":"secret"}`, 400)
	p.request(t, "DELETE", "/api/tailscale/settings", "{}", 404)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, tailnet := range []string{"one.example.com", "two.example.com"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := p.h.saveSettings(fields(`{"revision":1,"tailnet":"`+tailnet+`"}`), "admin")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		} else {
			checkError(t, err, 409)
		}
	}
	if success != 1 {
		t.Fatal("concurrent settings overwrite")
	}
}

func TestMissingOrDamagedKeyDoesNotOverwriteCredentials(t *testing.T) {
	p := newHarness(t)
	if _, err := p.h.saveSettings(fields(`{"revision":1,"tailnet":"-","api_token":"`+testToken+`"}`), "admin"); err != nil {
		t.Fatal(err)
	}
	_, ciphertext, _ := p.h.settings()
	keypath := filepath.Join(p.h.db.Directory, tokenKeyFile)
	key, err := os.ReadFile(keypath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(keypath); err != nil {
		t.Fatal(err)
	}
	_, err = p.h.decrypt(ciphertext)
	checkError(t, err, 500)
	_, err = p.h.saveSettings(fields(`{"revision":2,"tailnet":"-","api_token":"tskey-api-new"}`), "admin")
	checkError(t, err, 500)
	if _, err = os.Stat(keypath); !os.IsNotExist(err) {
		t.Fatal("missing key was recreated")
	}
	s, stillEncrypted, _ := p.h.settings()
	if s.Revision != 2 || stillEncrypted != ciphertext {
		t.Fatal("broken credential was overwritten")
	}
	if err = os.WriteFile(keypath, key, 0644); err != nil {
		t.Fatal(err)
	}
	_, err = p.h.decrypt(ciphertext)
	checkError(t, err, 500)
	if err = os.Chmod(keypath, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = p.h.decrypt("v1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	checkError(t, err, 500)
	_, err = p.h.decrypt("plaintext")
	checkError(t, err, 500)
	if _, err = credentials.Decrypt(p.h.db.Directory, tokenKeyFile, "other-purpose", ciphertext); err == nil {
		t.Fatal("wrong authentication context accepted")
	}
	if _, err = p.h.saveSettings(fields(`{"revision":2,"tailnet":"-","clear_api_token":true}`), "admin"); err != nil {
		t.Fatal(err)
	}
}

const deviceFixture = `{"devices":[{"nodeId":"nZ","hostname":"zeta","name":"zeta.example.ts.net","addresses":["100.64.0.2"],"os":"linux","tags":["tag:compute"],"authorized":true,"connectedToControl":true,"nodeKey":"private-node-key","endpoints":["private-endpoint"]},{"nodeId":"nA","hostname":"alpha","user":"alice@example.com","isExternal":true,"lastSeen":"2026-09-30T00:00:00Z"}]}`

func TestDeviceQueryAndConnectionTest(t *testing.T) {
	p := newHarness(t)
	p.login(t)
	p.request(t, "PUT", "/api/tailscale/settings", `{"revision":1,"tailnet":"alice@example.com","api_token":"`+testToken+`"}`, 200)
	calls := 0
	p.h.client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.URL.String() != apiBase+"/tailnet/alice@example.com/devices" || r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("invalid upstream request")
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Error("no request timeout")
		}
		return response(200, deviceFixture), nil
	})
	result := p.request(t, "GET", "/api/tailscale/devices", "", 200)
	raw := httpapi.JSONText(result)
	if strings.Contains(raw, "private-") {
		t.Fatal("unnecessary sensitive device fields exposed")
	}
	devices := result["devices"].([]any)
	if len(devices) != 2 || devices[0].(map[string]any)["nodeId"] != "nA" || devices[1].(map[string]any)["connectedToControl"] != true {
		t.Fatal(devices)
	}
	if devices[0].(map[string]any)["connectedToControl"] != nil {
		t.Fatal("unknown connection state inferred")
	}
	result = p.request(t, "POST", "/api/tailscale/test", "{}", 200)
	if result["device_count"] != float64(2) || result["devices"] != nil || result["revision"] != float64(2) || calls != 2 {
		t.Fatal(result)
	}
}

func TestUpstreamFailuresAreBoundedAndDoNotLeakCredentials(t *testing.T) {
	p := newHarness(t)
	for _, tc := range []struct {
		name string
		code int
		body string
		err  error
		want int
	}{
		{"expired", 401, testToken, nil, 502}, {"forbidden", 403, testToken, nil, 502},
		{"missing", 404, "", nil, 502}, {"rate limit", 429, "", nil, 503}, {"server", 500, testToken, nil, 502},
		{"bad json", 200, "<html>" + testToken, nil, 502}, {"missing devices", 200, `{}`, nil, 502},
		{"null devices", 200, `{"devices":null}`, nil, 502}, {"missing node ID", 200, `{"devices":[{}]}`, nil, 502},
		{"trailing body", 200, `{"devices":[]} {}`, nil, 502}, {"too large", 200, strings.Repeat(" ", maxResponseBytes+1), nil, 502},
		{"timeout", 0, "", context.DeadlineExceeded, 504}, {"transport", 0, "", errors.New(testToken), 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.h.client.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return response(tc.code, tc.body), nil
			})
			_, err := p.h.listDevices(context.Background(), Settings{Tailnet: "-"}, testToken)
			checkError(t, err, tc.want)
		})
	}
	calls := 0
	p.h.client.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		calls++
		r := response(302, "")
		r.Header.Set("Location", "https://attacker.invalid/")
		return r, nil
	})
	_, err := p.h.listDevices(context.Background(), Settings{Tailnet: "-"}, testToken)
	checkError(t, err, 502)
	if calls != 1 {
		t.Fatal("credential request followed redirect")
	}
	p.h.client.Transport = transportFunc(func(*http.Request) (*http.Response, error) { return response(200, `{"devices":[]}`), nil })
	devices, err := p.h.listDevices(context.Background(), Settings{Tailnet: "-"}, testToken)
	if err != nil || devices == nil || len(devices) != 0 {
		t.Fatal("valid empty network rejected")
	}
}

func TestDeviceInviteContractAndRedaction(t *testing.T) {
	p := newHarness(t)
	if _, err := p.h.saveSettings(fields(`{"revision":1,"tailnet":"-","api_token":"`+testToken+`"}`), "admin"); err != nil {
		t.Fatal(err)
	}
	p.h.client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer "+testToken || r.URL.Host != "api.tailscale.com" {
			t.Fatal("wrong upstream boundary")
		}
		switch r.Method {
		case "GET":
			return response(200, `[]`), nil
		case "POST":
			var body []map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body) != 1 || len(body[0]) != 2 || body[0]["multiUse"] != false || body[0]["allowExitNode"] != false {
				t.Fatalf("unsafe invite %v", body)
			}
			return response(200, `[{"id":"invite1","inviteUrl":"https://login.tailscale.com/admin/invite/test","multiUse":false}]`), nil
		case "DELETE":
			return response(404, `{"message":"already deleted"}`), nil
		}
		t.Fatal(r.Method)
		return nil, nil
	})
	if _, e := p.h.Invites(context.Background(), "node1"); e != nil {
		t.Fatal(e)
	}
	v, e := p.h.CreateInvite(context.Background(), "node1")
	if e != nil || v.ID != "invite1" {
		t.Fatalf("%+v %v", v, e)
	}
	if e = p.h.DeleteInvite(context.Background(), v.ID); e != nil {
		t.Fatal(e)
	}
	for _, status := range []int{401, 403, 429, 500} {
		p.h.client.Transport = transportFunc(func(*http.Request) (*http.Response, error) { return response(status, testToken), nil })
		_, e = p.h.CreateInvite(context.Background(), "node1")
		if e == nil || strings.Contains(e.Error(), testToken) || CreationRejected(e) != (status < 500) {
			t.Fatalf("status=%d err=%v", status, e)
		}
	}
}
