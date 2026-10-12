package rootless

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/containerorigin"
)

func TestAuditSourceEvidence(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, cgroup, uid string
		known             bool
	}{
		{"v2", "0::/system.slice/docker-" + id + ".scope\n", "1000", true},
		{"v1", "2:memory:/docker/" + id + "\n0::/user.slice\n", "1000", true},
		{"wrong-uid", "0::/docker/" + id + "\n", "2000", false},
		{"ambiguous", "0::/docker/" + id + "\n2:memory:/docker/" + strings.Repeat("b", 64) + "\n", "1000", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "123")
			os.Mkdir(dir, 0700)
			stat := "123 (docker (create)) S " + strings.Repeat("0 ", 18) + "4500"
			writeTestFile(t, dir+"/stat", stat, 0600)
			writeTestFile(t, dir+"/status", "Uid:\t"+tc.uid+"\t"+tc.uid+"\t"+tc.uid+"\t"+tc.uid+"\n", 0600)
			writeTestFile(t, dir+"/cgroup", tc.cgroup, 0600)
			source := containerorigin.Source{PID: 123, UID: 1000}
			(&creationAudit{procRoot: root}).readSource(&source)
			if tc.known {
				if source.Error != "" || source.ContainerID != id || source.StartTicks != 4500 {
					t.Fatal(source)
				}
			} else if source.Error == "" || source.ContainerID != "" {
				t.Fatal(source)
			}
			os.Remove(dir + "/stat")
			source = containerorigin.Source{PID: 123, UID: 1000}
			(&creationAudit{procRoot: root}).readSource(&source)
			if source.Error == "" || source.ContainerID != "" {
				t.Fatal("guessed vanished process", source)
			}
		})
	}
}

func auditBackend(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "alpha-audit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := dir + "/docker.sock"
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return socket
}

func TestCreationAuditRecordsOnlySuccessfulCreate(t *testing.T) {
	id, parent := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, tc := range []struct {
		name, method, path string
		status             int
		save               bool
	}{
		{"versioned", "POST", "/v1.48/containers/create", 201, true},
		{"unversioned", "POST", "/containers/create", 201, true},
		{"trailing-slash", "POST", "/v1.48/containers/create/", 201, true},
		{"conflict", "POST", "/containers/create", 409, false},
		{"start", "POST", "/containers/abc/start", 204, false},
		{"inspect", "GET", "/containers/abc/json", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := auditBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tc.method || r.URL.Path != tc.path {
					t.Error(r.Method, r.URL.Path)
				}
				if tc.method == "POST" {
					raw, _ := io.ReadAll(r.Body)
					if string(raw) != `{"Image":"test","Env":["SECRET=not-recorded"]}` {
						t.Error(string(raw))
					}
				}
				w.WriteHeader(tc.status)
				if tc.status != 204 {
					fmt.Fprintf(w, `{"Id":%q,"Warnings":[]}`, id)
				}
			}))
			dir := t.TempDir()
			audit := &creationAudit{store: originStore{directory: dir}, bootID: "boot"}
			body := `{"Image":"test","Env":["SECRET=not-recorded"]}`
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(body))
			source := containerorigin.Source{PID: 123, UID: 1000, StartTicks: 4500, ContainerID: parent}
			r = r.WithContext(context.WithValue(r.Context(), sourceKey{}, source))
			r.Header.Set("X-Alpha-User", "attacker")
			r.Header.Set("X-Container-ID", strings.Repeat("c", 64))
			w := httptest.NewRecorder()
			audit.proxy(upstream).ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatal(w.Code, w.Body.String())
			}
			records, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.save {
				if len(records) != 0 {
					t.Fatal(records)
				}
				return
			}
			reloaded := originStore{directory: dir}
			record, err := reloaded.read(id)
			if err != nil {
				t.Fatal(err)
			}
			if record.Source != source || record.ContainerID != id || record.Version != 1 || record.BootID != "boot" {
				t.Fatal(record)
			}
			raw, err := os.ReadFile(filepath.Join(dir, id+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte("SECRET")) || bytes.Contains(raw, []byte("attacker")) {
				t.Fatal("request secrets or untrusted identity persisted")
			}
			if err := reloaded.save(record); err == nil {
				t.Fatal("overwrote immutable provenance")
			}
		})
	}
}

func TestCreationAuditUsesUnixPeerAndSeparatesManagement(t *testing.T) {
	id := strings.Repeat("d", 64)
	upstream := auditBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201); fmt.Fprintf(w, `{"Id":%q}`, id) }))
	audit := &creationAudit{store: originStore{directory: t.TempDir()}, bootID: "boot", procRoot: "/proc"}
	dir, err := os.MkdirTemp("/tmp", "audit-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := dir + "/proxy.sock"
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: audit.proxy(upstream), ConnContext: audit.connectionContext}
	go server.Serve(listener)
	defer server.Close()
	transport := unixTransport(socket)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request, _ := http.NewRequest("POST", "http://docker/v1.48/containers/create", strings.NewReader(`{}`))
	request.Header.Set("X-Alpha-User", "forged")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 201 {
		t.Fatal(response.Status)
	}
	record, err := audit.store.read(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.Source.PID != int32(os.Getpid()) || record.Source.UID != uint32(os.Geteuid()) || record.Source.StartTicks == 0 || record.Source.Error != "" {
		t.Fatal(record)
	}
	for _, p := range []string{controlPath, containerorigin.Endpoint, "/_rootless/logs"} {
		response, err = client.Get("http://docker" + p)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Fatal("business socket exposed management", p, response.Status)
		}
	}
	w := httptest.NewRecorder()
	audit.store.serveHTTP(w, httptest.NewRequest("GET", containerorigin.Endpoint+"?container_id="+id, nil))
	var result containerorigin.Response
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Origins) != 1 || result.Origins[0].Source.PID != int32(os.Getpid()) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAuditSaveFailureDoesNotClaimSuccess(t *testing.T) {
	id := strings.Repeat("a", 64)
	upstream := auditBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201); fmt.Fprintf(w, `{"Id":%q}`, id) }))
	audit := &creationAudit{store: originStore{directory: t.TempDir() + "/missing"}, bootID: "boot"}
	w := httptest.NewRecorder()
	audit.proxy(upstream).ServeHTTP(w, httptest.NewRequest("POST", "/containers/create", strings.NewReader(`{}`)))
	if w.Code != 502 || !strings.Contains(w.Body.String(), id) || !strings.Contains(w.Body.String(), "已创建") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestOriginStoreRejectsCorruptionWithoutChangingData(t *testing.T) {
	id := strings.Repeat("a", 64)
	for i, raw := range []string{`{}`, `{"version":99}`, `{"version":1} {}`, `null`, `{broken`} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			store := originStore{directory: t.TempDir()}
			file := filepath.Join(store.directory, id+".json")
			writeTestFile(t, file, raw, 0600)
			if _, err := store.read(id); err == nil {
				t.Fatal("accepted corrupt record")
			}
			if got := readTestFile(t, file); got != raw {
				t.Fatal("changed corrupt record")
			}
			w := httptest.NewRecorder()
			store.serveHTTP(w, httptest.NewRequest("GET", containerorigin.Endpoint+"?container_id="+id, nil))
			if w.Code != 500 {
				t.Fatal(w.Code)
			}
		})
	}
	store := originStore{directory: t.TempDir()}
	for _, path := range []string{containerorigin.Endpoint, containerorigin.Endpoint + "?container_id=../bindings"} {
		w := httptest.NewRecorder()
		store.serveHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	target := filepath.Join(store.directory, "other")
	writeTestFile(t, target, "{}", 0600)
	if err := os.Symlink(target, filepath.Join(store.directory, id+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read(id); err == nil {
		t.Fatal("followed provenance symlink")
	}
}
