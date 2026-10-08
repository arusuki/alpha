package mihomo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"project-alpha/internal/platform"
)

func TestMain(m *testing.M) {
	if os.Getenv("ALPHA_MIHOMO_TEST_CORE") == "1" {
		os.Exit(fakeCore())
	}
	os.Exit(m.Run())
}

// A child process with the real core's flags and Unix API, so tests exercise
// process ownership, HTTP reloads, validation failures and persistent state.
func fakeCore() int {
	file := ""
	test := false
	for i, a := range os.Args {
		if a == "-t" {
			test = true
		}
		if a == "-f" && i+1 < len(os.Args) {
			file = os.Args[i+1]
		}
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return 2
	}
	config, groups, _, err := inspectConfig(raw)
	if err != nil || config["test-reject"] == true {
		return 3
	}
	if test {
		return 0
	}
	if config["test-exit"] == true {
		return 4
	}
	socket, _ := config["external-controller-unix"].(string)
	l, err := net.Listen("unix", socket)
	if err != nil {
		return 5
	}
	defer l.Close()
	var mu sync.Mutex
	selected := map[string]string{}
	populate := func(gs []Group) {
		groups = gs
		selected = map[string]string{}
		for _, g := range gs {
			selected[g.Name] = g.Proxies[0]
		}
	}
	populate(groups)
	server := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/version":
			fmt.Fprint(w, `{"version":"test"}`)
		case r.URL.Path == "/configs":
			var input struct {
				Payload string `json:"payload"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil {
				w.WriteHeader(400)
				return
			}
			c, gs, _, err := inspectConfig([]byte(input.Payload))
			if err != nil || c["test-reload-fail"] == true {
				w.WriteHeader(400)
				return
			}
			populate(gs)
			w.WriteHeader(204)
		case r.URL.Path == "/proxies":
			proxies := map[string]any{}
			for _, g := range groups {
				proxies[g.Name] = map[string]any{"all": g.Proxies, "now": selected[g.Name]}
			}
			json.NewEncoder(w).Encode(map[string]any{"proxies": proxies})
		case strings.HasPrefix(r.URL.Path, "/proxies/"):
			name := strings.TrimPrefix(r.URL.Path, "/proxies/")
			for _, g := range groups {
				if g.Name != name {
					continue
				}
				if r.Method == "GET" {
					json.NewEncoder(w).Encode(map[string]string{"now": selected[name]})
					return
				}
				var input struct {
					Name string `json:"name"`
				}
				if json.NewDecoder(r.Body).Decode(&input) != nil || !slices.Contains(g.Proxies, input.Name) {
					w.WriteHeader(400)
					return
				}
				selected[name] = input.Name
				w.WriteHeader(204)
				return
			}
			w.WriteHeader(404)
		default:
			w.WriteHeader(404)
		}
	})}
	if server.Serve(l) != nil {
		return 0
	}
	return 0
}

func testDB(t *testing.T) *platform.Database {
	t.Helper()
	dir, err := os.MkdirTemp("", "alpha-mh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	db, err := platform.OpenDatabase(dir, func(tx *sql.Tx) error {
		_, e := tx.Exec("INSERT INTO service_identity VALUES(1,'control',?)", platform.RandomHex(16))
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	return db
}
func fakeBundle(t *testing.T) Bundle {
	t.Helper()
	t.Setenv("ALPHA_MIHOMO_TEST_CORE", "1")
	c := testConfig()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c.Binary = binary
	p, err := Fetch(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(c, p, Target{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeLifecycleReloadSelectionAndRollback(t *testing.T) {
	db := testDB(t)
	m, err := New(db)
	must(t, err)
	defer m.Close()
	b := fakeBundle(t)
	ctx := context.Background()
	must(t, m.Apply(ctx, b))
	if m.Status(ctx).Running {
		t.Fatal("apply started a stopped service")
	}
	must(t, m.Select(ctx, "PROXY", "美国 A", "admin"))
	must(t, m.Service(ctx, "start", "admin"))
	s := m.Status(ctx)
	if !s.Running || !s.Ready || s.Groups[0].Now != "美国 A" {
		t.Fatalf("start %+v", s)
	}
	info, err := os.Stat(filepath.Join(db.Directory, "mihomo", "config.yaml"))
	must(t, err)
	if info.Mode().Perm() != 0600 {
		t.Fatal("config permissions")
	}
	raw, err := os.ReadFile(filepath.Join(db.Directory, "mihomo", "config.yaml"))
	must(t, err)
	if strings.Contains(string(raw), "external-controller:") {
		t.Fatal("TCP controller exposed")
	}
	if err = m.Select(ctx, "PROXY", "not-in-template", "admin"); err == nil {
		t.Fatal("selected unknown candidate")
	}
	for _, field := range []string{"test-reject", "test-reload-fail"} {
		bad := b
		bad.YAML += field + ": true\n"
		bad.Digest = bundleDigest(bad)
		if err = m.Apply(ctx, bad); err == nil {
			t.Fatal("accepted failing core configuration")
		}
		s = m.Status(ctx)
		if !s.Running || s.Digest != b.Digest || s.Groups[0].Now != "美国 A" {
			t.Fatalf("failed reload lost state %+v", s)
		}
	}
	updated := b
	updated.YAML += "ipv6: true\n"
	updated.Digest = bundleDigest(updated)
	must(t, m.Apply(ctx, updated))
	s = m.Status(ctx)
	if s.Groups[0].Now != "美国 A" || s.Digest != updated.Digest {
		t.Fatal("reload lost selection")
	}
	_, err = db.SQL.Exec("CREATE TRIGGER reject_runtime BEFORE UPDATE ON mihomo_runtime BEGIN SELECT RAISE(ABORT,'rejected'); END")
	must(t, err)
	if err = m.Apply(ctx, b); err == nil {
		t.Fatal("database failure ignored")
	}
	s = m.Status(ctx)
	if s.Digest != updated.Digest || s.Groups[0].Now != "美国 A" {
		t.Fatal("failed persistence did not roll back")
	}
	_, err = db.SQL.Exec("DROP TRIGGER reject_runtime")
	must(t, err)
	currentFile, err := os.ReadFile(filepath.Join(db.Directory, "mihomo", "config.yaml"))
	must(t, err)
	if !strings.Contains(string(currentFile), "ipv6: true") {
		t.Fatal("failed persistence did not restore generated file")
	}
	m.Close()
	m, err = New(db)
	must(t, err)
	defer m.Close()
	s = m.Status(ctx)
	if !s.Running || s.Groups[0].Now != "美国 A" {
		t.Fatalf("restart failed to recover %+v", s)
	}
	must(t, m.Service(ctx, "stop", "admin"))
	m.Close()
	m, err = New(db)
	must(t, err)
	defer m.Close()
	if m.Status(ctx).Running || m.Status(ctx).Enabled {
		t.Fatal("stop not persisted")
	}
	var persisted string
	must(t, db.SQL.QueryRow("SELECT bundle FROM mihomo_runtime").Scan(&persisted))
	if strings.Contains(persisted, "private-password") {
		t.Fatal("unencrypted database")
	}
}

func TestRuntimeMissingCoreAndEarlyExit(t *testing.T) {
	db := testDB(t)
	m, err := New(db)
	must(t, err)
	defer m.Close()
	b := fakeBundle(t)
	ctx := context.Background()
	if err = m.Apply(ctx, Bundle{}); err == nil {
		t.Fatal("empty deployment accepted")
	}
	missing := b
	missing.Binary = "/missing/mihomo"
	missing.Digest = bundleDigest(missing)
	if err = m.Apply(ctx, missing); err == nil {
		t.Fatal("missing binary accepted")
	}
	b.YAML += "test-exit: true\n"
	b.Digest = bundleDigest(b)
	must(t, m.Apply(ctx, b))
	if err = m.Service(ctx, "start", "admin"); err == nil {
		t.Fatal("exit reported as running")
	}
	s := m.Status(ctx)
	if s.Running || !s.Enabled || s.Error == "" {
		t.Fatalf("lost failure %+v", s)
	}
}

func TestStoreInheritanceConflictsAndMissingKey(t *testing.T) {
	db := testDB(t)
	s := Store{db}
	c := testConfig()
	must(t, s.Save("control", SaveSettings{Config: c}, "admin"))
	v, err := s.Settings("worker")
	must(t, err)
	if !v.Inherited || v.BaseRevision != 1 || v.Config.MixedPort != 7890 {
		t.Fatalf("inherit %+v", v)
	}
	c.MixedPort = 7990
	must(t, s.Save("worker", SaveSettings{BaseRevision: 1, Config: c}, "admin"))
	v, err = s.Settings("worker")
	must(t, err)
	if v.Inherited || v.Config.MixedPort != 7990 {
		t.Fatal("override ignored")
	}
	if err = s.Save("worker", SaveSettings{BaseRevision: 1, Config: c}, "admin"); err == nil {
		t.Fatal("stale write accepted")
	}
	c.MixedPort = 7991
	must(t, s.Save("control", SaveSettings{Revision: 1, BaseRevision: 1, Config: c}, "admin"))
	v, err = s.Settings("worker")
	must(t, err)
	if v.Config.MixedPort != 7990 {
		t.Fatal("override changed with control")
	}
	must(t, s.Save("worker", SaveSettings{Revision: 1, BaseRevision: 2, Inherit: true}, "admin"))
	v, err = s.Settings("worker")
	must(t, err)
	if !v.Inherited || v.Config.MixedPort != 7991 {
		t.Fatal("reset inheritance failed")
	}
	must(t, os.Remove(filepath.Join(db.Directory, "mihomo.key")))
	if _, err = s.Settings("control"); err == nil {
		t.Fatal("missing key ignored")
	}
	if err = s.Save("another", SaveSettings{Config: c}, "admin"); err == nil {
		t.Fatal("recreated encryption key")
	}
}

func TestControlDeliveryRetriesOfflineAndPreservesGoodBundle(t *testing.T) {
	db := testDB(t)
	m, err := New(db)
	must(t, err)
	defer m.Close()
	b := fakeBundle(t)
	cfg := testConfig()
	cfg.Binary = b.Binary
	c := &Control{Store: Store{db}, Local: m, force: map[string]bool{}}
	c.Targets = func() ([]Target, error) {
		return []Target{{ID: "control", Role: "control"}, {ID: "worker", Role: "worker"}, {ID: "registry", Role: "registry"}}, nil
	}
	online := false
	calls := map[string]int{}
	bundles := map[string]Bundle{}
	c.Remote = func(_ context.Context, target Target, method, path string, body, out any) error {
		calls[target.ID]++
		if !online {
			return fmt.Errorf("offline")
		}
		bundles[target.ID] = body.(Bundle)
		return nil
	}
	must(t, c.Store.Save("control", SaveSettings{Config: cfg}, "admin"))
	c.reconcile(context.Background())
	v, err := c.Store.Sync("registry")
	must(t, err)
	if v.Delivered || v.Error != "offline" || v.Bundle.Digest == "" {
		t.Fatal("lost pending delivery")
	}
	online = true
	c.reconcile(context.Background())
	v, err = c.Store.Sync("registry")
	must(t, err)
	if !v.Delivered || calls["registry"] != 2 || bundles["worker"].Digest != bundles["registry"].Digest {
		t.Fatal("retry or inheritance failed")
	}
	c.force["registry"] = true
	c.reconcile(context.Background())
	if calls["registry"] != 3 || calls["worker"] != 2 {
		t.Fatal("manual refresh did not re-deliver the selected target")
	}
	old := v.Bundle.Digest
	cfg.Template = "proxies: []\nproxy-groups: []"
	must(t, c.Store.Save("control", SaveSettings{Revision: 1, BaseRevision: 1, Config: cfg}, "admin"))
	c.reconcile(context.Background())
	v, err = c.Store.Sync("registry")
	must(t, err)
	if v.Bundle.Digest != old || v.Error == "" || calls["registry"] != 3 {
		t.Fatal("invalid template replaced working deployment")
	}
	// A restarted control reuses persisted deliveries and does not re-fetch
	// subscriptions until due. No independent health polling is involved.
	if time.Since(time.Unix(int64(v.AttemptedAt), 0)) > time.Minute {
		t.Fatal("missing attempt time")
	}
}
