package gpu

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

func mockRootlessDocker(t *testing.T, ids ...string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \" $* \" in\n *' info '*) printf '%s\\n' '[\"name=rootless\"]';;\n *' ps '*) printf '%s\\n' '" + strings.Join(ids, "\n") + "';;\n *) exit 1;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func TestRootlessManualOwnership(t *testing.T) {
	m := testMonitor(t)
	parent, a, b := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	mockRootlessDocker(t, a, b)
	if _, err := m.db.SQL.Exec("INSERT INTO owners VALUES(?,'alice'); INSERT INTO managed_containers VALUES(?,'primary')", parent, parent); err != nil {
		t.Fatal(err)
	}
	now := platform.Now()
	if _, err := m.db.SQL.Exec(`INSERT INTO gpu_intervals VALUES('gpu','GPU',?,?,50,'["rootless Docker"]')`, now-15, now); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := SetRootlessOwners(ctx, m.db, []string{a, b}, "alice", "admin", true); err != nil {
		t.Fatal(err)
	}
	if rows, err := RootlessOwnerships(m.db); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	for range 2 {
		if err := SetRootlessOwners(ctx, m.db, []string{a, b}, "alice", "admin", false); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := RootlessOwnerships(m.db)
	if err != nil || len(rows) != 2 || rows[0].Owner != "alice" || rows[1].OwnerContainerID != parent {
		t.Fatal(rows, err)
	}
	owners, _, err := readContainerOwners(m.db)
	if err != nil || owners[a] != "alice" || owners[b] != "alice" {
		t.Fatal(owners, err)
	}
	devices := []Device{{Processes: []Process{{Owner: rootlessOwner, ContainerID: a}, {Owner: rootlessOwner, ContainerID: b}}}}
	if warning := readContainerOrigins(ctx, devices, owners, "/missing/audit.sock"); warning != "" {
		t.Fatal(warning)
	}
	if got := deviceOwners(devices[0]); !reflect.DeepEqual(got, []string{"alice"}) {
		t.Fatal(got)
	}
	// Both reassigning and releasing the primary apply without rewriting children.
	for _, name := range []string{"bob", ""} {
		if _, err := m.db.SQL.Exec("UPDATE owners SET owner=? WHERE container_id=?", name, parent); err != nil {
			t.Fatal(err)
		}
		owners, _, err = readContainerOwners(m.db)
		if err != nil || owners[a] != name {
			t.Fatal(owners, err)
		}
		devices[0].Processes[0].Owner = rootlessOwner
		devices[0].Processes[1].Owner = rootlessOwner
		if warning := readContainerOrigins(ctx, devices, owners, "/missing/audit.sock"); warning != "" {
			t.Fatal(warning)
		}
		want := name
		if want == "" {
			want = rootlessOwner
		}
		if got := deviceOwners(devices[0]); !reflect.DeepEqual(got, []string{want}) {
			t.Fatal(got)
		}
	}
	var history string
	if err := m.db.SQL.QueryRow("SELECT owners FROM gpu_intervals").Scan(&history); err != nil || history != `["rootless Docker"]` {
		t.Fatal(history, err)
	}
	// Removal works for containers already deleted or while Docker is unavailable.
	t.Setenv("PATH", t.TempDir())
	if err := SetRootlessOwners(ctx, m.db, []string{a}, "", "admin", false); err != nil {
		t.Fatal(err)
	}
	if rows, err := RootlessOwnerships(m.db); err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
}

func TestRootlessOwnershipRejectsInvalidTargetsAndRollsBack(t *testing.T) {
	m := testMonitor(t)
	parent, a, b := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	mockRootlessDocker(t, a, b, parent)
	if _, err := m.db.SQL.Exec("INSERT INTO owners VALUES(?,'alice')", parent); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ids   []string
		owner string
	}{
		{nil, "alice"}, {[]string{"short"}, "alice"}, {[]string{a}, "missing"}, {[]string{parent}, "alice"}, {[]string{a, strings.Repeat("d", 64)}, "alice"},
	} {
		if err := SetRootlessOwners(context.Background(), m.db, tc.ids, tc.owner, "admin", false); err == nil {
			t.Fatal("invalid accepted", tc)
		}
	}
	if _, err := m.db.SQL.Exec("CREATE TRIGGER reject_owner_audit BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT,'failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := SetRootlessOwners(context.Background(), m.db, []string{a, b}, "alice", "admin", false); err == nil {
		t.Fatal("expected rollback")
	}
	if rows, err := RootlessOwnerships(m.db); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' '[\"name=seccomp\"]'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if _, err := RootlessContainers(context.Background(), m.db); err == nil {
		t.Fatal("rootful engine accepted")
	}
}

func TestRootlessOwnershipAPIAuthorization(t *testing.T) {
	m := testMonitor(t)
	for _, method := range []string{"PUT", "DELETE"} {
		_, _, err := m.Dispatch(httptest.NewRecorder(), httptest.NewRequest(method, "/api/gpu/rootless-owners", strings.NewReader(`{}`)), platform.User{Role: "viewer"})
		if err == nil {
			t.Fatal("viewer may edit")
		}
	}
	status, _, err := m.Dispatch(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/gpu/rootless-owners", nil), platform.User{Role: "viewer"})
	if err != nil || status != 200 {
		t.Fatal(status, err)
	}
	parent, a := strings.Repeat("a", 64), strings.Repeat("b", 64)
	mockRootlessDocker(t, a)
	if _, err := m.db.SQL.Exec("INSERT INTO owners VALUES(?,'alice')", parent); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"PUT", "DELETE"} {
		body := `{"container_ids":["` + a + `"],"owner":"alice"}`
		request := httptest.NewRequest(method, "/api/gpu/rootless-owners", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		status, _, err = m.Dispatch(httptest.NewRecorder(), request, platform.User{Role: "admin", Username: "admin"})
		if err != nil || status != 200 {
			t.Fatal(status, err)
		}
		rows, err := RootlessOwnerships(m.db)
		if err != nil || len(rows) != map[string]int{"PUT": 1, "DELETE": 0}[method] {
			t.Fatal(rows, err)
		}
	}
}
