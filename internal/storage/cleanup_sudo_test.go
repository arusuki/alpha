package storage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const cleanupTestPassword = "test-only-sudo-password"

// A subprocess impersonates only sudo's pipe protocol; no real sudo credentials
// or elevated deletion are used by this test suite.
func TestSudoCleanupProcess(t *testing.T) {
	args := os.Args
	index := -1
	for i, arg := range args {
		if arg == "--" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	mode := args[index+1]
	if strings.Join(args[index+2:], " ") == "" {
		os.Exit(2)
	}
	input := bufio.NewReader(os.Stdin)
	if mode != "nopass" {
		fmt.Fprint(os.Stderr, sudoCleanupPrompt[:9])
		fmt.Fprint(os.Stderr, sudoCleanupPrompt[9:])
		password, err := input.ReadString('\n')
		if err != nil || password != cleanupTestPassword+"\n" {
			os.Exit(3)
		}
		if mode == "bad" {
			fmt.Fprintln(os.Stderr, "diagnostic reflected "+password)
			fmt.Fprint(os.Stderr, sudoCleanupPrompt)
			_, _ = input.ReadString('\n')
			os.Exit(1)
		}
	}
	if mode == "waiting" {
		time.Sleep(10 * time.Second)
		os.Exit(1)
	}
	if mode == "disconnect" {
		// Read one batch, then prove the control pipe closes on cancellation.
		fmt.Fprintln(os.Stdout, `{"type":"ready"}`)
		_, _ = input.ReadString('\n')
		_, _ = input.ReadByte()
		os.Exit(0)
	}
	if err := serveCleanupHelper(context.Background(), input, os.Stdout); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}
func fakeSudoCommand(t *testing.T, mode string) func(context.Context, string, ...string) *exec.Cmd {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "/usr/bin/sudo" || len(args) != 7 || args[0] != "-S" || args[1] != "-k" || args[2] != "-p" || args[3] != sudoCleanupPrompt || args[4] != "--" || args[6] != "cleanup-helper" {
			t.Error("unexpected sudo command contract")
		}
		if strings.Contains(strings.Join(args, " "), cleanupTestPassword) {
			t.Error("password leaked into argv")
		}
		argv := append([]string{"-test.run=^TestSudoCleanupProcess$", "--", mode}, args...)
		return exec.CommandContext(ctx, os.Args[0], argv...)
	}
}
func TestSudoCleanupOneBatchAndNoSecretPersistence(t *testing.T) {
	for _, mode := range []string{"password", "nopass", "bad"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "target")
			os.Mkdir(target, 0700)
			os.WriteFile(filepath.Join(target, "data"), []byte("temporary"), 0600)
			password := []byte(cleanupTestPassword)
			rows := []string{}
			err := runSudoCleanup(context.Background(), password, cleanupHelperRequest{Paths: []string{target}}, func(path, status, message string) error { rows = append(rows, path+status+message); return nil }, fakeSudoCommand(t, mode))
			if !bytes.Equal(password, make([]byte, len(password))) {
				t.Fatal("password was not wiped")
			}
			if mode == "bad" {
				if err == nil || strings.Contains(err.Error(), cleanupTestPassword) || len(rows) != 0 {
					t.Fatal("authentication failure leaked data or ran deletion")
				}
				if _, err := os.Stat(target); err != nil {
					t.Fatal("deleted after failed sudo authentication")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || rows[0] != target+"deleted" {
					t.Fatal(rows)
				}
				if _, err := os.Stat(target); !os.IsNotExist(err) {
					t.Fatal("target remains", err)
				}
			}
		})
	}
}
func TestSudoCleanupCancellationClearsSecretAndStopsHelper(t *testing.T) {
	for _, mode := range []string{"waiting", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			password := []byte(cleanupTestPassword)
			started := time.Now()
			err := runSudoCleanup(ctx, password, cleanupHelperRequest{Paths: []string{filepath.Join(t.TempDir(), "keep")}}, func(string, string, string) error { t.Error("unexpected result"); return nil }, fakeSudoCommand(t, mode))
			if err == nil || time.Since(started) > 4*time.Second {
				t.Fatal("cancellation did not stop helper", err)
			}
			if !bytes.Equal(password, make([]byte, len(password))) {
				t.Fatal("password retained after cancellation")
			}
		})
	}
}

func TestSudoCleanupReportsFailingPathAndReason(t *testing.T) {
	for _, kind := range []string{"missing", "nested-permission"} {
		t.Run(kind, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "target")
			failedPath, reason := target, unix.ENOENT.Error()
			if kind == "nested-permission" {
				if os.Geteuid() == 0 {
					t.Skip("root bypasses directory permissions")
				}
				nested := filepath.Join(target, "nested")
				if err := os.MkdirAll(nested, 0700); err != nil {
					t.Fatal(err)
				}
				failedPath, reason = filepath.Join(nested, "keep"), unix.EACCES.Error()
				if err := os.WriteFile(failedPath, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(nested, 0500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(nested, 0700) })
			}
			count := 0
			err := runSudoCleanup(context.Background(), []byte(cleanupTestPassword), cleanupHelperRequest{Paths: []string{target}}, func(path, status, message string) error {
				count++
				if path != target || status != "failed" || !strings.Contains(message, failedPath) || !strings.Contains(message, reason) || !strings.Contains(message, "可能已删除部分内容") {
					t.Errorf("failure lost its path or cause: %q %q %q", path, status, message)
				}
				if strings.Contains(message, cleanupTestPassword) {
					t.Error("password leaked into deletion error")
				}
				return nil
			}, fakeSudoCommand(t, "password"))
			if err != nil || count != 1 {
				t.Fatalf("expected one failed result, count=%d error=%v", count, err)
			}
		})
	}
}
func TestPrivilegedCleanupRevalidatesWholeBatch(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "keep")
	os.Mkdir(target, 0700)
	request := cleanupHelperRequest{Paths: []string{target, "/etc"}}
	password := []byte(cleanupTestPassword)
	statuses := []string{}
	err := runSudoCleanup(context.Background(), password, request, func(path, status, message string) error { statuses = append(statuses, status); return nil }, fakeSudoCommand(t, "password"))
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[0] != "failed" || statuses[1] != "failed" {
		t.Fatal(statuses)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("partially deleted an invalid batch")
	}
}
func TestCleanupHelperStopsOnParentPipeEOF(t *testing.T) {
	input, writer := io.Pipe()
	output, reader := io.Pipe()
	defer input.Close()
	defer writer.Close()
	defer output.Close()
	defer reader.Close()
	done := make(chan error, 1)
	go func() { done <- serveCleanupHelper(context.Background(), input, reader) }()
	var event cleanupHelperEvent
	if err := json.NewDecoder(output).Decode(&event); err != nil || event.Type != "ready" {
		t.Fatal(err)
	}
	writer.Close() // service dies before supplying the confirmed batch
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted missing batch")
		}
	case <-time.After(time.Second):
		t.Fatal("helper survived parent pipe EOF")
	}
}

func TestSudoCleanupDoesNotFallBackToUnmountedUpperLayer(t *testing.T) {
	root := t.TempDir()
	upper, host := filepath.Join(root, "diff"), filepath.Join(root, "host-cache")
	cache := filepath.Join(upper, "root/.cache")
	for _, path := range []string{cache, host} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "keep"), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	request := cleanupHelperRequest{Paths: []string{host, cache}, WritableLayers: []string{upper}}
	count := 0
	err := runSudoCleanup(context.Background(), []byte(cleanupTestPassword), request, func(path, status, message string) error {
		count++
		if status != "failed" || !strings.Contains(message, "合并挂载") || !strings.Contains(message, "本批未执行删除") || strings.Contains(message, "可能已删除部分内容") {
			t.Errorf("unexpected preflight result: %s %s", status, message)
		}
		return nil
	}, fakeSudoCommand(t, "password"))
	if err != nil || count != 2 {
		t.Fatal(count, err)
	}
	for _, path := range []string{cache, host} {
		if _, err := os.Stat(filepath.Join(path, "keep")); err != nil {
			t.Fatal("deleted data before overlay preflight finished", err)
		}
	}
}
