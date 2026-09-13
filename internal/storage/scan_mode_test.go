package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
)

func TestScanConfiguration(t *testing.T) {
	for _, backend := range []string{"auto", "host", "docker"} {
		for _, mode := range []string{"normal", "fast"} {
			want := defaultConfig()
			want.ScanBackend, want.ScanMode = backend, mode
			c, err := parseConfig([]byte(httpapi.JSONText(want)))
			if err != nil || c.ScanMode != mode || c.ScanBackend != backend {
				t.Fatalf("config=%+v err=%v", c, err)
			}
		}
	}
	for _, field := range []string{"scan_mode", "scan_backend"} {
		for _, invalid := range []any{"", "turbo", true, 4, nil, []string{"fast"}} {
			var raw object
			json.Unmarshal([]byte(httpapi.JSONText(defaultConfig())), &raw)
			raw[field] = invalid
			if _, err := parseConfig([]byte(httpapi.JSONText(raw))); err == nil {
				t.Fatalf("accepted invalid %s: %v", field, invalid)
			}
		}
	}
	var raw object
	json.Unmarshal([]byte(httpapi.JSONText(defaultConfig())), &raw)
	for field := range raw {
		value := raw[field]
		delete(raw, field)
		if _, err := parseConfig([]byte(httpapi.JSONText(raw))); err == nil {
			t.Fatalf("accepted missing %s", field)
		}
		raw[field] = value
	}
	raw["unexpected"] = "fast"
	if _, err := parseConfig([]byte(httpapi.JSONText(raw))); err == nil {
		t.Fatal("accepted unknown field")
	}
}

func TestScanModeCPUBudgets(t *testing.T) {
	for _, available := range []int{0, 1, 2, 4, 8, 128, 1024} {
		for _, mode := range []string{"normal", "fast"} {
			n := scanModeParallelism(mode, available)
			if n < 1 || n > max(1, available) {
				t.Fatalf("invalid CPU budget: %s %d -> %d", mode, available, n)
			}
			if mode == "fast" && n != max(1, available) {
				t.Fatal("fast left available CPUs unused")
			}
			if mode != "fast" && n > 4 {
				t.Fatal("normal exceeded four CPUs")
			}
			if helperPIDLimit(n) < 128 || helperPIDLimit(n) < 2*n+32 {
				t.Fatal("insufficient runtime thread headroom")
			}
		}
	}
}

func TestScanModesPersistAndReachWorker(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	historical := p.configure()
	parentParallelism := runtime.GOMAXPROCS(0)
	t.Setenv("GOMAXPROCS", "1")
	for i, mode := range []string{"fast", "normal"} {
		c := historical
		c.ScanMode = mode
		p.expect(200, "PUT", "/api/settings", object{"revision": i + 2, "value": c}, nil)
		saved := p.expect(200, "GET", "/api/settings", nil, nil)
		if saved["value"].(map[string]any)["scan_mode"] != mode {
			t.Fatal("mode not persisted")
		}
		// Queued jobs use the saved scan mode while retaining their scan scope.
		job, err := p.m.startPlan("administrator", "manual", scanPlan{Config: historical})
		if err != nil {
			t.Fatal(err)
		}
		record := waitJob(t, p.db, job["id"].(string))
		if record["status"] != "completed" {
			t.Fatalf("scan failed: %v", record)
		}
		progress := record["progress"].(map[string]any)
		if progress["scan_mode"] != mode || progress["gomaxprocs"] != float64(c.scanParallelism()) {
			t.Fatalf("worker runtime ignored mode %s: %v", mode, progress)
		}
		if record["config"].(map[string]any)["scan_mode"] != mode {
			t.Fatal("job did not retain its mode")
		}
		if runtime.GOMAXPROCS(0) != parentParallelism {
			t.Fatal("scan changed web service parallelism")
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			p.m.mu.Lock()
			err := p.m.reapLocked()
			idle := p.m.process == nil
			p.m.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if idle {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("worker did not exit")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// Opt in only on a Linux Docker host with the helper image already installed.
// The regular suite never grants itself Docker access or downloads an image.
func TestDockerHelperScanModesIntegration(t *testing.T) {
	if os.Getenv("PROJECT_ALPHA_TEST_DOCKER_MODES") != "1" {
		t.Skip("set PROJECT_ALPHA_TEST_DOCKER_MODES=1 to test real read-only containers")
	}
	root := t.TempDir()
	const files = 32768
	for i := 0; i < 128; i++ {
		branch := filepath.Join(root, strconv.Itoa(i))
		if err := os.Mkdir(branch, 0700); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < files/128; j++ {
			mustWrite(t, filepath.Join(branch, strconv.Itoa(j)), nil)
		}
	}
	var normalAllocated int64
	for _, mode := range []string{"normal", "fast"} {
		t.Run(mode, func(t *testing.T) {
			c := defaultConfig()
			c.ScanMode, c.MaxNodes = mode, 1000
			lease := filepath.Join(t.TempDir(), "lease.json")
			ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), helperLeaseKey{}, lease), 90*time.Second)
			defer cancel()
			observedParallelism, peakThreads, checkedLimit := 0, 0, false
			startedAt := time.Now()
			result, started, err := scanViaDocker(ctx, helperRequest{Version: 1, Config: c, Paths: []string{root}, Mounts: mountTable()}, func(progress object) error {
				if n, ok := progress["gomaxprocs"].(json.Number); ok {
					observedParallelism, _ = strconv.Atoi(string(n))
				}
				if checkedLimit {
					return nil
				}
				raw, err := os.ReadFile(lease)
				if os.IsNotExist(err) {
					return nil
				}
				if err != nil {
					return err
				}
				var token string
				if json.Unmarshal(raw, &token) != nil || !helperTokenPattern.MatchString(token) {
					return fmt.Errorf("invalid helper lease")
				}
				out, err := runDocker(ctx, []string{"container", "inspect", "--format", "{{.State.Pid}} {{.HostConfig.PidsLimit}}", "project-alpha-scan-" + token}, 5)
				if err != nil {
					return err
				}
				var pid, limit int
				if _, err := fmt.Sscan(out, &pid, &limit); err != nil {
					return err
				}
				if limit != helperPIDLimit(c.scanParallelism()) {
					return fmt.Errorf("incorrect PID limit: %d", limit)
				}
				status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
				if err != nil {
					return err
				}
				for _, line := range strings.Split(string(status), "\n") {
					if strings.HasPrefix(line, "Threads:") {
						peakThreads, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Threads:")))
					}
				}
				checkedLimit = true
				return nil
			})
			if err != nil || !started || result == nil {
				t.Fatalf("helper failed: %v", err)
			}
			if result.ErrorCount != 0 || result.Tree.Files != files {
				t.Fatalf("incomplete scan: errors=%d files=%d", result.ErrorCount, result.Tree.Files)
			}
			if !checkedLimit || peakThreads <= 0 || peakThreads >= helperPIDLimit(c.scanParallelism()) || observedParallelism != c.scanParallelism() {
				t.Fatalf("runtime budget mismatch: checked=%v threads=%d gomaxprocs=%d", checkedLimit, peakThreads, observedParallelism)
			}
			if mode == "normal" {
				normalAllocated = result.Tree.Allocated
			} else if result.Tree.Allocated != normalAllocated {
				t.Fatal("mode changed physical accounting")
			}
			if _, err := os.Stat(lease); !os.IsNotExist(err) {
				t.Fatalf("helper was not cleaned: %v", err)
			}
			t.Logf("files=%d GOMAXPROCS=%d PID limit=%d sampled threads=%d elapsed=%s", result.Tree.Files, observedParallelism, helperPIDLimit(c.scanParallelism()), peakThreads, time.Since(startedAt))
		})
	}
}

func TestScanCLIModes(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "data"), make([]byte, 8192))
	previous := runtime.GOMAXPROCS(0)
	for _, mode := range []string{"normal", "fast"} {
		output := filepath.Join(t.TempDir(), "snapshot.json")
		if err := ScanCLI(context.Background(), []string{"--scan-mode", mode, "--no-docker", "--root", root, "--output", output}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot Snapshot
		if json.Unmarshal(raw, &snapshot) != nil || snapshot.Tree == nil || snapshot.Tree.Files != 1 || snapshot.Tree.Allocated < 8192 {
			t.Fatal("mode changed scan accounting")
		}
		if runtime.GOMAXPROCS(0) != previous {
			t.Fatal("CLI did not restore runtime parallelism")
		}
	}
	if err := ScanCLI(context.Background(), []string{"--scan-mode", "turbo"}); err == nil {
		t.Fatal("CLI accepted invalid mode")
	}
}
