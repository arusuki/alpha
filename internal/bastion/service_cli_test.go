package bastion

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeShareServiceTools(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"systemctl", "journalctl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestShareServiceCommands(t *testing.T) {
	fakeShareServiceTools(t, "printf '%s\\n' \"$@\"\nprintf 'service output\\n'\nprintf 'diagnostic\\n' >&2\n")
	for _, test := range []struct {
		args []string
		want []string
	}{
		{[]string{"status"}, []string{"status", proxyUnitName, "--no-pager", "--full"}},
		{[]string{"log"}, []string{"-u", proxyUnitName, "--no-pager", "-n", "50"}},
		{[]string{"log", "-n", "100", "-f"}, []string{"-u", proxyUnitName, "--no-pager", "-n", "100", "--follow"}},
		{[]string{"log", "--lines", "0", "--follow"}, []string{"-u", proxyUnitName, "--no-pager", "-n", "0", "--follow"}},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			var out bytes.Buffer
			if err := InitializeCLI(context.Background(), test.args, &out); err != nil {
				t.Fatal(err)
			}
			want := strings.Join(test.want, "\n") + "\nservice output\n"
			if !strings.HasPrefix(out.String(), want) || !strings.Contains(out.String(), "diagnostic\n") {
				t.Fatalf("unexpected command or missing output: %q", out.String())
			}
		})
	}
}

func TestShareServiceHelpAndInvalidOptionsDoNotRunTools(t *testing.T) {
	fakeShareServiceTools(t, "printf 'tool invoked\\n'\n")
	for _, action := range []string{"status", "log"} {
		var out bytes.Buffer
		if err := InitializeCLI(context.Background(), []string{action, "--help"}, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "project-alpha share-node "+action) || strings.Contains(out.String(), "tool invoked") {
			t.Fatal(out.String())
		}
	}
	for _, args := range [][]string{
		{"status", "extra"}, {"status", "--uninstall"}, {"status", "-f"},
		{"log", "extra"}, {"log", "--serve"}, {"log", "--no-service"},
		{"log", "-n", "-1"}, {"log", "--lines", "invalid"}, {"log", "-n"},
	} {
		var out bytes.Buffer
		if err := InitializeCLI(context.Background(), args, &out); err == nil {
			t.Fatalf("accepted invalid options: %v", args)
		}
		if strings.Contains(out.String(), "tool invoked") {
			t.Fatalf("ran tool with invalid options: %v", args)
		}
	}
}

func TestShareServiceErrorsPreserveOutput(t *testing.T) {
	fakeShareServiceTools(t, "printf 'inactive (dead)\\n'\nprintf 'service unavailable\\n' >&2\nexit 3\n")
	for _, action := range []string{"status", "log"} {
		var out bytes.Buffer
		err := InitializeCLI(context.Background(), []string{action}, &out)
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
			t.Fatalf("lost command failure: %v", err)
		}
		if !strings.Contains(out.String(), "inactive (dead)") || !strings.Contains(out.String(), "service unavailable") {
			t.Fatalf("lost failure output: %q", out.String())
		}
	}
}

func TestShareServiceMissingTools(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for action, tool := range map[string]string{"status": "systemctl", "log": "journalctl"} {
		err := InitializeCLI(context.Background(), []string{action}, &bytes.Buffer{})
		if !errors.Is(err, exec.ErrNotFound) || !strings.Contains(err.Error(), tool) {
			t.Fatalf("missing tool error unclear: %v", err)
		}
	}
}

type cancelShareLogWriter struct {
	buffer bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelShareLogWriter) Write(p []byte) (int, error) {
	n, err := w.buffer.Write(p)
	w.cancel()
	return n, err
}

func TestShareLogStreamsAndStopsOnCancellation(t *testing.T) {
	fakeShareServiceTools(t, "printf 'live log entry\\n'\nwhile :; do :; done\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := &cancelShareLogWriter{cancel: cancel}
	err := InitializeCLI(ctx, []string{"log", "-f"}, out)
	if !errors.Is(err, context.Canceled) || out.buffer.String() != "live log entry\n" {
		t.Fatalf("log did not stream before exit or stop on cancellation: %q %v", out.buffer.String(), err)
	}
}
