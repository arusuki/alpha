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

func TestWorkerStartupToken(t *testing.T)   { testServiceStartupToken(t, "worker") }
func TestRegistryStartupToken(t *testing.T) { testServiceStartupToken(t, "registry") }

func testServiceStartupToken(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("REG_PASS", "Abcd1234")
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
		{name: "automatic after explicit override", generated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PROJECT_ALPHA_"+strings.ToUpper(mode)+"_TOKEN", tc.env)
			args := []string{"--" + mode, "--data-dir", directory, "--port", "0", "--tetragon-socket", filepath.Join(t.TempDir(), "missing.sock")}
			if tc.file != "" {
				path := filepath.Join(t.TempDir(), "token")
				mustWrite(t, path, []byte(tc.file))
				args = append(args, "--"+mode+"-token-file", path)
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
						t.Errorf("%s stopped: %v", mode, err)
					}
				case <-time.After(15 * time.Second):
					t.Errorf("%s did not stop", mode)
				}
			}()
			addressPattern := regexp.MustCompile(`project alpha: (http://\S+) \(` + mode + `\)`)
			tokenPattern := regexp.MustCompile(mode + ` token \(saved in data directory\): ([a-f0-9]{64})\n`)
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
					if strings.Contains(string(raw), token) || strings.Contains(string(raw), mode+" token") {
						t.Fatal("explicit token printed in startup log")
					}
				}
				if address != "" && token != "" {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if address == "" || token == "" {
				t.Fatalf("%s did not print its address and generated token", mode)
			}
			if tc.generated {
				if previousToken != "" && token != previousToken {
					t.Fatal("restart changed the persisted token")
				}
				stored, err := readServiceToken(directory, mode)
				if err != nil || stored != token {
					t.Fatalf("saved token differs from startup token: %v", err)
				}
				info, err := os.Stat(filepath.Join(directory, mode+"-token"))
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("%s token permissions: %v", mode, err)
				}
				previousToken = token
			}
			client := &http.Client{Timeout: 3 * time.Second}
			for _, credential := range []string{"", strings.Repeat("x", 64), token} {
				req, err := http.NewRequest("GET", address+"/api/"+mode+"/info", nil)
				if err != nil {
					t.Fatal(err)
				}
				if credential != "" {
					req.Header.Set("Authorization", "Bearer "+credential)
				}
				req.Header.Set("X-Alpha-Control", strings.Repeat("a", 32))
				response, err := client.Do(req)
				if mode == "registry" && credential != token {
					if err == nil {
						response.Body.Close()
						t.Fatal("registry accepted invalid token")
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				want := http.StatusUnauthorized
				if credential == token {
					want = http.StatusOK
				}
				if response.StatusCode != want {
					t.Fatalf("%s authentication: got %d, want %d", mode, response.StatusCode, want)
				}
			}
		})
	}
}

func TestRegistryRejectsInvalidSavedToken(t *testing.T) {
	t.Setenv("PROJECT_ALPHA_REGISTRY_TOKEN", "")
	t.Setenv("REG_PASS", "Abcd1234")
	for _, kind := range []string{"invalid contents", "insecure permissions", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "registry-token")
			var err error
			switch kind {
			case "invalid contents":
				err = os.WriteFile(path, []byte("invalid"), 0600)
			case "insecure permissions":
				err = os.WriteFile(path, []byte(strings.Repeat("s", 64)), 0600)
				if err == nil {
					err = os.Chmod(path, 0644)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				mustWrite(t, target, []byte(strings.Repeat("s", 64)))
				err = os.Symlink(target, path)
			case "directory":
				err = os.Mkdir(path, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			contents, _ := os.ReadFile(path)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := Run(ctx, []string{"--registry", "--data-dir", directory, "--port", "0"}); err == nil || !strings.Contains(err.Error(), "registry token") {
				t.Fatalf("invalid saved token accepted: %v", err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("saved token was replaced: %v", err)
			}
			raw, _ := os.ReadFile(path)
			if string(raw) != string(contents) {
				t.Fatal("saved token was overwritten")
			}
		})
	}
}

func TestWorkerRejectsInvalidExplicitToken(t *testing.T) {
	testServiceRejectsInvalidExplicitToken(t, "worker")
}
func TestRegistryRejectsInvalidExplicitToken(t *testing.T) {
	testServiceRejectsInvalidExplicitToken(t, "registry")
}

func testServiceRejectsInvalidExplicitToken(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("REG_PASS", "Abcd1234")
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
			t.Setenv("PROJECT_ALPHA_"+strings.ToUpper(mode)+"_TOKEN", tc.env)
			directory := t.TempDir()
			args := []string{"--" + mode, "--data-dir", directory, "--port", "0"}
			if tc.useFile {
				path := filepath.Join(t.TempDir(), "token")
				if !tc.missing {
					mustWrite(t, path, []byte(tc.file))
				}
				args = append(args, "--"+mode+"-token-file", path)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := Run(ctx, args); err == nil || !strings.Contains(err.Error(), mode+" token") {
				t.Fatalf("expected %s token error, got %v", mode, err)
			}
			if _, err := os.Stat(filepath.Join(directory, mode+"-token")); !os.IsNotExist(err) {
				t.Fatal("invalid explicit token triggered automatic generation")
			}
		})
	}
}

func TestServiceTokenConcurrentCreationAndInvalidFile(t *testing.T) {
	const mode = "worker"
	directory := t.TempDir()
	var workers sync.WaitGroup
	tokens := make(chan string, 8)
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			token, err := loadServiceToken(directory, mode)
			if err != nil {
				t.Error(err)
				return
			}
			tokens <- token
		}()
	}
	workers.Wait()
	close(tokens)
	want, err := readServiceToken(directory, mode)
	if err != nil {
		t.Fatal(err)
	}
	for token := range tokens {
		if token != want {
			t.Fatal("concurrent startups returned different tokens")
		}
	}
	path := filepath.Join(directory, mode+"-token")
	if err := os.WriteFile(path, []byte("invalid-saved-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadServiceToken(directory, mode); err == nil {
		t.Fatal("invalid saved token was replaced")
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "invalid-saved-token" {
		t.Fatal("invalid saved token was overwritten")
	}
}
