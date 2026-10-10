package updater

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Runs in a child because successful handoff replaces the process image.
func TestServiceUpdateProcess(t *testing.T) {
	if path := os.Getenv("ALPHA_TEST_SERVICE_PLAN"); path != "" {
		if err := service(context.Background(), path, io.Discard); err != nil {
			os.Exit(17)
		}
		os.Exit(18)
	}
}
func TestServiceFailureRestartsAndRecoveryMarkerStopsRestart(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "safe-failure", true: "uncertain-install"}[pending], func(t *testing.T) {
			db := oldDatabase(t, "registry")
			bin := t.TempDir()
			exe := filepath.Join(bin, "project-alpha")
			marker := filepath.Join(bin, "restarted")
			// A missing prepared release fails without making a network call.
			writeFile(t, exe, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'project-alpha dev'; else printf '%s\\n' \"$@\" > '"+marker+"'; fi\n"))
			if pending {
				os.WriteFile(filepath.Join(bin, ".alpha-update-pending"), []byte("recovery required"), 0600)
			}
			plan := filepath.Join(db.Directory, "update-service.json")
			if err := WriteJSON(plan, ServicePlan{Format: 3, Directory: db.Directory, Executable: exe, Role: "registry", Repo: "arusuki/alpha", Arguments: []string{"serve", "--registry", "--data-dir", db.Directory}}); err != nil {
				t.Fatal(err)
			}
			child := exec.Command(os.Args[0], "-test.run=^TestServiceUpdateProcess$")
			child.Env = append(os.Environ(), "ALPHA_TEST_SERVICE_PLAN="+plan)
			output, err := child.CombinedOutput()
			if pending != (err != nil) {
				t.Fatalf("pending=%v: %v %s", pending, err, output)
			}
			raw, e := os.ReadFile(marker)
			if pending {
				if !os.IsNotExist(e) {
					t.Fatal("restarted with uncertain installation")
				}
			} else if e != nil || !strings.Contains(string(raw), "--registry\n--data-dir\n"+db.Directory) {
				t.Fatalf("lost restart arguments: %s %v", raw, e)
			}
			raw, e = os.ReadFile(filepath.Join(db.Directory, "update-result.json"))
			if e != nil {
				t.Fatal(e)
			}
			var result ServiceResult
			if json.Unmarshal(raw, &result) != nil || result.State != "failed" || result.Error == "" {
				t.Fatalf("missing failure result: %s", raw)
			}
			lock, e := db.LockService()
			if e != nil {
				t.Fatalf("updater retained service lock: %v", e)
			}
			lock.Close()
		})
	}
}

func TestExplicitHTTPProxyIsUsedForGitHub(t *testing.T) {
	db := oldDatabase(t, "registry")
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "project-alpha"), script("project-alpha", "v0.5.4"))
	seen := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen <- r.Method + " " + r.Host; w.WriteHeader(502) }))
	defer proxy.Close()
	err := Run(context.Background(), []string{"--data-dir", db.Directory, "--bin-dir", bin, "--check", "--http-proxy", proxy.URL}, io.Discard)
	if err == nil {
		t.Fatal("unexpected download through failing proxy")
	}
	select {
	case request := <-seen:
		if request != "CONNECT api.github.com:443" {
			t.Fatal(request)
		}
	default:
		t.Fatal("configured proxy was bypassed")
	}
}
