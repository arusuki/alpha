package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/cluster"
	"project-alpha/internal/platform"
	"project-alpha/internal/updater"
	"project-alpha/internal/updates"
)

func TestUpdaterHandoffReleasedLock(t *testing.T) {
	directory := os.Getenv("ALPHA_TEST_HANDOFF_DIRECTORY")
	if directory == "" {
		return
	}
	db, err := platform.OpenExistingDatabase(directory, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	if os.Getenv("ALPHA_TEST_PREPARING") == "1" {
		if lock, err := db.LockService(); err == nil {
			lock.Close()
			t.Fatal("service released its lock before download")
		}
		if err := os.WriteFile(filepath.Join(directory, "prepare-started"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		ready := false
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(filepath.Join(directory, "prepare-release")); err == nil {
				ready = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !ready {
			t.Fatal("preparation was never released")
		}
		path := filepath.Join(directory, "update-service.json")
		p, err := updater.ReadServicePlan(path)
		if err != nil {
			t.Fatal(err)
		}
		stage, err := os.MkdirTemp(filepath.Dir(p.Executable), ".alpha-stage-")
		if err != nil {
			t.Fatal(err)
		}
		p.Prepared = &updater.PreparedUpdate{Stage: stage, Tag: "v0.3.3", Installed: "v0.3.2"}
		if err := updater.WriteJSON(path, p); err != nil {
			t.Fatal(err)
		}
		return
	}
	lock, err := db.LockService()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	raw, err := os.ReadFile(filepath.Join(directory, "update-service.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan updater.ServicePlan
	if err = json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Role != "worker" || plan.Proxy != "http://127.0.0.1:7890" || plan.Repo != "arusuki/alpha" || len(plan.Arguments) == 0 || plan.Arguments[0] != "serve" {
		t.Fatalf("invalid handoff: %+v", plan)
	}
	if err = os.WriteFile(filepath.Join(directory, "handoff-checked"), []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestWebUpdateClosesServiceBeforeExec(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "data")
	binary := filepath.Join(root, "project-alpha")
	build := exec.Command("go", "build", "-ldflags=-X project-alpha/internal/buildinfo.Version=v0.3.2", "-o", binary, "./cmd/project-alpha")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	helper := filepath.Join(root, "alpha-updater")
	// Test executable checks the real process's released lock after exec.
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nif [ \"$1\" = --service-protocol ]; then echo 2; exit 0; fi\nif [ \"$1\" = _prepare ]; then export ALPHA_TEST_PREPARING=1; fi\nexec " + quote(os.Args[0]) + " -test.run=^TestUpdaterHandoffReleasedLock$\n"
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	logfile, err := os.Create(filepath.Join(root, "service.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logfile.Close()
	token := strings.Repeat("u", 32)
	cmd := exec.Command(binary, "serve", "--worker", "--data-dir", directory, "--port", "0", "--tetragon-socket", filepath.Join(root, "absent.sock"))
	cmd.Stdout = logfile
	cmd.Stderr = logfile
	cmd.Env = append(os.Environ(), "PROJECT_ALPHA_WORKER_TOKEN="+token, "ALPHA_TEST_HANDOFF_DIRECTORY="+directory)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var address string
	pattern := regexp.MustCompile(`project alpha: (http://127\.0\.0\.1:\d+)`)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		raw, _ := os.ReadFile(logfile.Name())
		if match := pattern.FindStringSubmatch(string(raw)); match != nil {
			address = match[1]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if address == "" {
		raw, _ := os.ReadFile(logfile.Name())
		t.Fatalf("service not started: %s", raw)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	request := func(method, path, body, id string) *http.Response {
		t.Helper()
		r, _ := http.NewRequest(method, address+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Alpha-Node", id)
		r.Header.Set("X-Alpha-User", `{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","username":"admin","role":"admin"}`)
		r.Header.Set("Content-Type", "application/json")
		res, e := client.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		return res
	}
	response := request("GET", "/api/worker/info", "", "")
	var info cluster.Info
	json.NewDecoder(response.Body).Decode(&info)
	response.Body.Close()
	raw, _ := json.Marshal(map[string]any{"revision": 1, "config": updates.Config{Command: helper, Repo: "arusuki/alpha", Proxy: "http://127.0.0.1:7890"}})
	response = request("PUT", updates.Path+"/settings", string(raw), info.ID)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("settings: %s", body)
	}
	response = request("POST", updates.Path+"/update", "{}", info.ID)
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 202 {
		t.Fatalf("update: %s", body)
	}
	ready := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(filepath.Join(directory, "prepare-started")); err == nil {
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("background preparation did not start")
	}
	response = request("GET", updates.Path+"/health", "", info.ID)
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(body), `"update_state":"downloading"`) {
		t.Fatalf("service unavailable during download: %s", body)
	}
	if err := os.WriteFile(filepath.Join(directory, "prepare-release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			raw, _ := os.ReadFile(logfile.Name())
			t.Fatalf("handoff: %v %s", err, raw)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("service did not hand off")
	}
	checked, err := os.ReadFile(filepath.Join(directory, "handoff-checked"))
	if err != nil || string(checked) != fmt.Sprint(cmd.Process.Pid) {
		t.Fatalf("handoff did not retain process or release lock: %s %v", checked, err)
	}
}
