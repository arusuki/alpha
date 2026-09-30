package app

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWorkerStartupToken(t *testing.T) {
	directory := t.TempDir()
	var previousToken string
	for _, tc := range []struct {
		name, env, file, token string
		generated              bool
	}{
		{name: "generated", generated: true},
		{name: "reused on restart", generated: true},
		{name: "environment", env: strings.Repeat("e", 32), token: strings.Repeat("e", 32)},
		{name: "file overrides environment", env: strings.Repeat("e", 32), file: strings.Repeat("f", 32) + "\n", token: strings.Repeat("f", 32)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PROJECT_ALPHA_WORKER_TOKEN", tc.env)
			args := []string{"--worker", "--data-dir", directory, "--port", "0", "--tetragon-socket", filepath.Join(t.TempDir(), "missing.sock")}
			if tc.file != "" {
				path := filepath.Join(t.TempDir(), "token")
				mustWrite(t, path, []byte(tc.file))
				args = append(args, "--worker-token-file", path)
			}
			output, err := os.CreateTemp(t.TempDir(), "startup.log")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			oldOutput := log.Writer()
			log.SetOutput(output)
			defer log.SetOutput(oldOutput)
			ctx, cancel := context.WithCancel(context.Background())
			stopped := make(chan error, 1)
			go func() { stopped <- Run(ctx, args) }()
			defer func() {
				cancel()
				select {
				case err := <-stopped:
					if err != nil {
						t.Errorf("worker stopped: %v", err)
					}
				case <-time.After(15 * time.Second):
					t.Error("worker did not stop")
				}
			}()
			addressPattern := regexp.MustCompile(`project alpha: (http://\S+) \(worker\)`)
			tokenPattern := regexp.MustCompile(`worker token \(saved in data directory\): ([a-f0-9]{64})\n`)
			var address, token string
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				raw, err := os.ReadFile(output.Name())
				if err != nil {
					t.Fatal(err)
				}
				if match := addressPattern.FindSubmatch(raw); match != nil {
					address = string(match[1])
				}
				if tc.generated {
					if match := tokenPattern.FindSubmatch(raw); match != nil {
						token = string(match[1])
					}
				} else {
					token = tc.token
					if strings.Contains(string(raw), token) || strings.Contains(string(raw), "worker token") {
						t.Fatal("explicit token printed in startup log")
					}
				}
				if address != "" && token != "" {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if address == "" || token == "" {
				t.Fatal("worker did not print its address and generated token")
			}
			if tc.generated {
				if previousToken != "" && token != previousToken {
					t.Fatal("restart changed the persisted token")
				}
				stored, err := readWorkerToken(directory)
				if err != nil || stored != token {
					t.Fatalf("saved token differs from startup token: %v", err)
				}
				info, err := os.Stat(filepath.Join(directory, workerTokenName))
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("worker token permissions: %v", err)
				}
				previousToken = token
			}
			client := &http.Client{Timeout: 3 * time.Second}
			for _, credential := range []string{"", strings.Repeat("x", 64), token} {
				req, err := http.NewRequest("GET", address+"/api/worker/info", nil)
				if err != nil {
					t.Fatal(err)
				}
				if credential != "" {
					req.Header.Set("Authorization", "Bearer "+credential)
				}
				response, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				want := http.StatusUnauthorized
				if credential == token {
					want = http.StatusOK
				}
				if response.StatusCode != want {
					t.Fatalf("worker authentication: got %d, want %d", response.StatusCode, want)
				}
			}
		})
	}
}

func TestWorkerRejectsInvalidExplicitToken(t *testing.T) {
	for _, tc := range []struct {
		name, env, file  string
		useFile, missing bool
	}{
		{name: "short environment", env: "short"},
		{name: "whitespace environment", env: strings.Repeat(" ", 32)},
		{name: "empty file", useFile: true, env: strings.Repeat("e", 32)},
		{name: "short file", useFile: true, file: "short"},
		{name: "missing file", useFile: true, missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PROJECT_ALPHA_WORKER_TOKEN", tc.env)
			args := []string{"--worker", "--data-dir", t.TempDir(), "--port", "0"}
			if tc.useFile {
				path := filepath.Join(t.TempDir(), "token")
				if !tc.missing {
					mustWrite(t, path, []byte(tc.file))
				}
				args = append(args, "--worker-token-file", path)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := Run(ctx, args); err == nil || !strings.Contains(err.Error(), "worker token") {
				t.Fatalf("expected worker token error, got %v", err)
			}
		})
	}
}

func TestWorkerTokenConcurrentCreationAndInvalidFile(t *testing.T) {
	directory := t.TempDir()
	var workers sync.WaitGroup
	tokens := make(chan string, 8)
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			token, err := loadWorkerToken(directory)
			if err != nil {
				t.Error(err)
				return
			}
			tokens <- token
		}()
	}
	workers.Wait()
	close(tokens)
	want, err := readWorkerToken(directory)
	if err != nil {
		t.Fatal(err)
	}
	for token := range tokens {
		if token != want {
			t.Fatal("concurrent startups returned different tokens")
		}
	}
	path := filepath.Join(directory, workerTokenName)
	if err := os.WriteFile(path, []byte("invalid-saved-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWorkerToken(directory); err == nil {
		t.Fatal("invalid saved token was replaced")
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "invalid-saved-token" {
		t.Fatal("invalid saved token was overwritten")
	}
}
