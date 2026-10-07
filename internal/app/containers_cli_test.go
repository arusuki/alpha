package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

func TestContainersImportCLI(t *testing.T) {
	root := t.TempDir()
	directory, base := filepath.Join(root, "platform"), filepath.Join(root, "docker-data")
	mounts := []map[string]any{}
	for _, dest := range []string{"workspace", "home", "data"} {
		source := filepath.Join(base, "alice", dest)
		if dest == "data" {
			source = filepath.Join(base, dest)
		}
		if err := os.MkdirAll(source, 0700); err != nil {
			t.Fatal(err)
		}
		mounts = append(mounts, map[string]any{"Type": "bind", "Source": source, "Destination": "/" + dest, "RW": true})
	}
	id := strings.Repeat("a", 64)
	container := map[string]any{
		"Id": id, "Name": "/alice", "Image": "sha256:test",
		"Config": map[string]any{"Image": "training:test", "Hostname": "docker-alice", "Tty": true},
		"HostConfig": map[string]any{
			"NetworkMode": "bridge", "IpcMode": "host", "RestartPolicy": map[string]string{"Name": "unless-stopped"},
			"Ulimits":        []map[string]any{{"Name": "memlock", "Soft": -1, "Hard": -1}},
			"DeviceRequests": []map[string]any{{"Driver": "nvidia", "Count": -1, "Capabilities": [][]string{{"gpu"}}}},
			"PortBindings":   map[string]any{"22/tcp": []map[string]string{{"HostPort": "2222"}}},
		},
		"State": map[string]any{"Running": true, "Status": "running"}, "Mounts": mounts,
	}
	raw, _ := json.Marshal([]any{container})
	inspectPath := filepath.Join(root, "inspect.json")
	mustWrite(t, inspectPath, raw)
	t.Setenv("IMPORT_TEST_INSPECT", inspectPath)
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	t.Setenv("PROJECT_ALPHA_DATA_DIR", directory)
	script := `#!/bin/sh
test "$1" = --host || exit 9
shift 2
case "$1" in
info) printf '%s\n' '{"ID":"daemon-test","OSType":"linux"}' ;;
ps) printf '%s\n' '` + id + `' ;;
container) cat "$IMPORT_TEST_INSPECT" ;;
exec) test "$3" = /usr/sbin/sshd && test "$4" = -T || exit 9; printf 'port 22\n' ;;
*) exit 9 ;;
esac
`
	if err := os.WriteFile(filepath.Join(root, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"containers", "import", "--base-dir", base, "--dry-run"},
		{"containers", "import", "--data-dir", directory, "--base-dir", base},
		{"containers", "import"},
	} {
		if err := Run(context.Background(), args); err != nil {
			t.Fatal(err)
		}
	}
	db, err := platform.OpenDatabase(directory, Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	var count int
	if err := db.SQL.QueryRow("SELECT COUNT(*) FROM managed_containers").Scan(&count); err != nil || count != 1 {
		t.Fatalf("records: %d %v", count, err)
	}
	var owner string
	if err := db.SQL.QueryRow("SELECT owner FROM owners WHERE container_id=?", id).Scan(&owner); err != nil || owner != "alice" {
		t.Fatalf("owner: %q %v", owner, err)
	}
	if err := db.SQL.QueryRow("SELECT COUNT(*) FROM audit WHERE actor='cli' AND action='container.adopt'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit: %d %v", count, err)
	}
}

func TestContainersImportRejectsOldData(t *testing.T) {
	dir := t.TempDir()
	db, err := platform.OpenDatabase(dir, Initialize)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.SQL.Exec("PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	db.SQL.Close()
	err = Run(context.Background(), []string{"containers", "import", "--data-dir", dir})
	if err == nil || !strings.Contains(err.Error(), "retained upgrade window") || !strings.Contains(err.Error(), "existing data preserved") {
		t.Fatalf("expected format error: %v", err)
	}
	db, err = platform.OpenExistingDatabase(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	var version int
	if err = db.SQL.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("rejected database was modified: version=%d err=%v", version, err)
	}
}
