package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"project-alpha/internal/platform"
	"project-alpha/internal/storage"
)

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestServiceCrashStopsWorkerAndDocker(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "platform")
	db, err := platform.OpenDatabase(directory, Initialize)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.SQL.Exec("UPDATE settings SET value=json_set(value,'$.interval_minutes',5,'$.schedule_mode','interval') WHERE id=1"); err != nil {
		db.SQL.Close()
		t.Fatal(err)
	}
	db.SQL.Close()
	pidfile := filepath.Join(root, "pids")
	mustWrite(t, filepath.Join(root, "docker"), []byte("#!/bin/sh\nsleep 60 &\necho $PPID $! > '"+pidfile+"'\nwait\n"))
	os.Chmod(filepath.Join(root, "docker"), 0700)
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	executable := filepath.Join(t.TempDir(), "project-alpha")
	build := exec.Command("go", "build", "-o", executable, "./cmd/project-alpha")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build service: %v: %s", err, output)
	}
	t.Setenv("PROJECT_ALPHA_WORKER_TOKEN", strings.Repeat("w", 32))
	server := exec.Command(executable, "serve", "--worker", "--data-dir", directory, "--port", "0")
	logfile, err := os.Create(filepath.Join(root, "service.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logfile.Close()
	server.Stdout = logfile
	server.Stderr = logfile
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { server.Process.Kill(); server.Wait() }()
	var worker, child int
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(pidfile)
		if err == nil {
			fmt.Sscan(string(raw), &worker, &child)
			if worker > 0 && child > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if worker == 0 || child == 0 {
		raw, _ := os.ReadFile(logfile.Name())
		t.Fatalf("worker did not launch: %s", raw)
	}
	defer syscall.Kill(-worker, syscall.SIGKILL)
	if err = server.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	server.Wait()
	stopped := func(pid int) bool {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		return os.IsNotExist(err) || (err == nil && strings.Contains(string(raw), ") Z "))
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if stopped(worker) && stopped(child) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !stopped(worker) || !stopped(child) {
		t.Fatal("service crash left scanner or Docker descendant running")
	}
	db, err = platform.OpenDatabase(directory, Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	manager, err := storage.NewManager(storage.NewStore(db))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	jobs, err := platform.Rows(db.SQL, "SELECT * FROM jobs")
	if err != nil || len(jobs) != 1 || jobs[0]["status"] != "interrupted" {
		t.Fatalf("restart recovery: %v %v", jobs, err)
	}
}
