package rootless

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHostLoopbackHelperProcess(t *testing.T) {
	port := os.Getenv("ROOTLESS_TEST_PORT")
	if port == "" {
		return
	}
	conn, err := net.DialTimeout("tcp", "10.0.2.2:"+port, 3*time.Second)
	if err != nil {
		if os.Getenv("ROOTLESS_TEST_EXPECT") == "blocked" {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = conn.Write([]byte("loopback-test"))
	var reply [11]byte
	if err == nil {
		_, err = io.ReadFull(conn, reply[:])
	}
	conn.Close()
	if err != nil || string(reply[:]) != "loopback-ok" || os.Getenv("ROOTLESS_TEST_EXPECT") == "blocked" {
		fmt.Fprintln(os.Stderr, "unexpected loopback result", err, string(reply[:]))
		os.Exit(1)
	}
	os.Exit(0)
}
func TestRealHostLoopback(t *testing.T) {
	if os.Getenv("ROOTLESS_NETWORK_INTEGRATION") != "1" {
		t.Skip("set ROOTLESS_NETWORK_INTEGRATION=1 for real rootlesskit/slirp4netns network checks")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	responses := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			responses <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		var b [13]byte
		_, err = io.ReadFull(conn, b[:])
		if err == nil && string(b[:]) != "loopback-test" {
			err = fmt.Errorf("unexpected request: %q", b)
		}
		if err == nil {
			_, err = conn.Write([]byte("loopback-ok"))
		}
		responses <- err
	}()
	for _, expected := range []string{"blocked", "allowed"} {
		args := []string{"--net=slirp4netns", "--state-dir=" + t.TempDir() + "/state"}
		if expected == "blocked" {
			args = append(args, "--disable-host-loopback")
		}
		args = append(args, os.Args[0], "-test.run=^TestHostLoopbackHelperProcess$")
		cmd := exec.Command("rootlesskit", args...)
		cmd.Env = append(os.Environ(), "ROOTLESS_TEST_PORT="+port, "ROOTLESS_TEST_EXPECT="+expected)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pid := cmd.Process.Pid
		timer := time.AfterFunc(15*time.Second, func() { syscall.Kill(-pid, syscall.SIGKILL) })
		err = cmd.Wait()
		timer.Stop()
		syscall.Kill(-pid, syscall.SIGKILL)
		if err != nil {
			t.Fatalf("%s: %v %s", expected, err, strings.TrimSpace(out.String()))
		}
	}
	select {
	case err := <-responses:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing host loopback request")
	}
}
