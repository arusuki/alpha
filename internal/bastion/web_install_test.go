package bastion

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

const installTestPassword = "test-only-install-password"

func TestInstallSudoProcess(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "--" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	mode := os.Args[index+1]
	input := bufio.NewReader(os.Stdin)
	if mode != "nopass" {
		fmt.Fprint(os.Stderr, sudoInstallPrompt[:7])
		fmt.Fprint(os.Stderr, sudoInstallPrompt[7:])
		password, err := input.ReadString('\n')
		if err != nil || password != installTestPassword+"\n" {
			os.Exit(3)
		}
		if mode == "bad" {
			fmt.Fprintln(os.Stderr, "PAM echoed: "+password)
			fmt.Fprint(os.Stderr, sudoInstallPrompt)
			_, _ = input.ReadString('\n')
			os.Exit(1)
		}
	}
	if mode == "waiting" {
		time.Sleep(10 * time.Second)
		os.Exit(1)
	}
	if err := serveInstallHelper(context.Background(), input, os.Stdout, func(ctx context.Context, request installRequest) error {
		if request.ServiceUser != "control" || request.Directory != "/test/control" {
			os.Exit(4)
		}
		if mode == "disconnect" {
			<-ctx.Done()
			return ctx.Err()
		}
		if mode == "failed" {
			return fmt.Errorf("test SSH configuration conflict")
		}
		return nil
	}); err != nil {
		os.Exit(5)
	}
	os.Exit(0)
}

func fakeInstallCommand(t *testing.T, mode string) func(context.Context, string, ...string) *exec.Cmd {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "/usr/bin/sudo" || len(args) != 8 || strings.Join(args[:5], " ") != "-S -k -p "+sudoInstallPrompt+" --" || args[6] != "bastion" || args[7] != "install-helper" {
			t.Error("unexpected sudo contract")
		}
		if strings.Contains(strings.Join(args, " "), installTestPassword) {
			t.Error("password in argv")
		}
		return exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestInstallSudoProcess$", "--", mode}, args...)...)
	}
}

func TestWebInstallSudoPasswordLifecycle(t *testing.T) {
	for _, mode := range []string{"password", "nopass", "bad", "failed", "waiting", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			if mode == "waiting" || mode == "disconnect" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
			}
			password := []byte(installTestPassword)
			err := runSudoInstall(ctx, password, installRequest{Action: "init", ServiceUser: "control", Directory: "/test/control"}, fakeInstallCommand(t, mode))
			if !bytes.Equal(password, make([]byte, len(password))) {
				t.Fatal("password was not wiped")
			}
			if mode == "password" || mode == "nopass" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || strings.Contains(err.Error(), installTestPassword) {
				t.Fatalf("failed operation leaked or succeeded: %v", err)
			}
			if mode == "failed" && !strings.Contains(err.Error(), "SSH configuration conflict") {
				t.Fatalf("lost installer error: %v", err)
			}
		})
	}
}

func TestInstallHelperDisconnectStopsRootWork(t *testing.T) {
	input, write := io.Pipe()
	defer input.Close()
	output, read := io.Pipe()
	defer output.Close()
	defer read.Close()
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		done <- serveInstallHelper(context.Background(), input, read, func(ctx context.Context, _ installRequest) error { close(started); <-ctx.Done(); return ctx.Err() })
	}()
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() || scanner.Text() != `{"type":"ready"}` {
		t.Fatal("missing ready")
	}
	if err := json.NewEncoder(write).Encode(installRequest{Action: "init", ServiceUser: "control", Directory: "/test/control"}); err != nil {
		t.Fatal(err)
	}
	<-started
	write.Close()
	if !scanner.Scan() {
		t.Fatal("helper did not report cancellation")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("root work survived closed input")
	}
}

func TestWebInstallAPIProtectsParametersAndAudit(t *testing.T) {
	h, _, _ := fixture(t)
	h.installInfo = func() webInstallInfo { return webInstallInfo{ServiceUser: "control", Available: true} }
	var received []byte
	calls := 0
	h.installRunner = func(_ context.Context, password []byte, request installRequest) error {
		calls++
		if request.ServiceUser != "control" || request.Directory != h.DB.Directory {
			t.Fatal("client controlled installation target")
		}
		if string(password) != installTestPassword {
			t.Fatal("wrong password")
		}
		received = password
		return nil
	}
	call := func(body, role string) error {
		r := httptest.NewRequest("POST", "/api/bastion/install", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		_, _, err := h.Dispatch(httptest.NewRecorder(), r, platform.User{Username: "operator", Role: role})
		return err
	}
	valid := `{"action":"init","sudo_password":"` + installTestPassword + `"}`
	for _, body := range []string{`{}`, `{"action":"init","sudo_password":""}`, `{"action":"init","sudo_password":123}`, `{"action":"init","sudo_password":"a\nb"}`, `{"action":"init","sudo_password":"a","service_user":"root"}`, `{"action":"init","sudo_password":"a","directory":"/etc"}`, `{"action":"init","sudo_password":"` + strings.Repeat("a", 1025) + `"}`} {
		if err := call(body, "admin"); err == nil {
			t.Fatal("accepted invalid install input")
		}
	}
	if err := call(valid, "viewer"); err == nil {
		t.Fatal("viewer installed")
	}
	h.installMu.Lock()
	err := call(valid, "admin")
	h.installMu.Unlock()
	var api *httpapi.Error
	if !errors.As(err, &api) || api.Status != 409 || calls != 0 {
		t.Fatal("duplicate install accepted", err, calls)
	}
	if err = call(valid, "admin"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !bytes.Equal(received, make([]byte, len(received))) {
		t.Fatal("password retained after install")
	}
	rows, err := platform.Rows(h.DB.SQL, "SELECT actor,action,detail FROM audit WHERE action LIKE 'bastion.account.%' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	text := httpapi.JSONText(rows)
	if len(rows) != 2 || strings.Contains(text, installTestPassword) || !strings.Contains(text, "bastion.account.init.completed") {
		t.Fatal("bad audit", text)
	}
}

func TestWebInstallRequiresAdminSessionAndCSRF(t *testing.T) {
	h, _, _ := fixture(t)
	h.installInfo = func() webInstallInfo { return webInstallInfo{ServiceUser: "control", Available: true} }
	calls := 0
	h.installRunner = func(context.Context, []byte, installRequest) error { calls++; return nil }
	server := platform.NewServer(h.DB, h, nil, nil, false)
	type login struct{ token, csrf string }
	users := map[string]login{}
	for _, role := range []string{"admin", "viewer"} {
		fields := map[string]json.RawMessage{"username": json.RawMessage(`"` + role + `"`), "password": json.RawMessage(`"test-platform-password"`), "role": json.RawMessage(`"` + role + `"`)}
		if _, err := h.DB.CreateUser(fields, "test", role == "admin"); err != nil {
			t.Fatal(err)
		}
		token, session, err := h.DB.Login(role, "test-platform-password")
		if err != nil {
			t.Fatal(err)
		}
		users[role] = login{token, session.CSRF}
	}
	for _, test := range []struct {
		name, role, origin string
		csrf               bool
		status             int
	}{
		{"anonymous", "", "http://localhost", false, 401},
		{"missing csrf", "admin", "http://localhost", false, 403},
		{"cross origin", "admin", "https://untrusted.test", true, 403},
		{"viewer", "viewer", "http://localhost", true, 403},
		{"authorized", "admin", "http://localhost", true, 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://localhost/api/bastion/install", strings.NewReader(`{"action":"init","sudo_password":"`+installTestPassword+`"}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", test.origin)
			if test.role != "" {
				user := users[test.role]
				r.AddCookie(&http.Cookie{Name: "project_alpha_session", Value: user.token})
				if test.csrf {
					r.Header.Set("X-CSRF-Token", user.csrf)
				}
			}
			before := calls
			w := httptest.NewRecorder()
			server.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
			if test.status != 200 && calls != before {
				t.Fatal("unauthorized request invoked sudo")
			}
			if strings.Contains(w.Body.String(), installTestPassword) || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("credential exposure or cacheable response")
			}
		})
	}
	if calls != 1 {
		t.Fatalf("installer called %d times", calls)
	}
}
