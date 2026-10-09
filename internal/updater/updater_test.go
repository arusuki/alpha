package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/platform"
)

func oldDatabase(t *testing.T, role string) *platform.Database {
	t.Helper()
	dir := t.TempDir()
	raw, err := sql.Open("sqlite3", filepath.Join(dir, "platform.sqlite3")+"?_foreign_keys=on&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	modules := []string{"platform"}
	switch role {
	case "control":
		modules = append(modules, "agent", "members", "bastion", "tailscale", "cluster")
	case "worker":
		modules = append(modules, "storage", "containers", "container_ownership", "gpu")
	case "registry":
		modules = append(modules, "registry")
	}
	for _, m := range modules {
		b, e := os.ReadFile("testdata/v0.5.0/" + m + ".sql")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = raw.Exec(string(b)); e != nil {
			t.Fatal(m, e)
		}
	}
	if _, err = raw.Exec("INSERT INTO service_identity VALUES(1,?,'persistent-instance'); PRAGMA user_version=36", role); err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec("INSERT INTO users VALUES('user','admin','existing-password-hash','admin',1,123)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	return &platform.Database{SQL: raw, Directory: dir}
}
func script(name, version string) []byte {
	return []byte("#!/bin/sh\nprintf '%s\\n' '" + name + " " + version + "'\n")
}
func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0755); err != nil {
		t.Fatal(err)
	}
}
func buildTarget(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "alpha-updater")
	cmd := exec.Command("go", "build", "-ldflags=-X project-alpha/internal/buildinfo.Version=v0.6.0", "-o", path, "../../cmd/alpha-updater")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build target updater: %v: %s", err, b)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func makeArchive(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for name, data := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func releaseServer(t *testing.T, target []byte, badHash bool) github {
	t.Helper()
	packageName := "project-alpha_v0.6.0_linux_" + runtime.GOARCH
	archive := makeArchive(t, map[string][]byte{
		packageName + "/bin/project-alpha":   script("project-alpha", "v0.6.0"),
		packageName + "/bin/alpha-updater":   target,
		packageName + "/bin/rootless-docker": script("rootless-docker", "v0.6.0"),
	})
	hash := sha256.Sum256(archive)
	if badHash {
		hash[0] ^= 1
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/arusuki/alpha/releases/latest":
			json.NewEncoder(w).Encode(release{Tag: "v0.6.0", Assets: []asset{{Name: packageName + ".tar.gz", URL: server.URL + "/assets/archive"}, {Name: "SHA256SUMS", URL: server.URL + "/assets/sums"}}})
		case "/assets/archive":
			w.Write(archive)
		case "/assets/sums":
			fmt.Fprintf(w, "%x  %s.tar.gz\n", hash, packageName)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return github{server.Client(), server.URL, "arusuki/alpha", ""}
}
func assertVersion(t *testing.T, db *platform.Database, want int) {
	t.Helper()
	var got int
	if err := db.SQL.QueryRow("PRAGMA user_version").Scan(&got); err != nil || got != want {
		t.Fatalf("schema=%d want=%d err=%v", got, want, err)
	}
	var hash string
	if err := db.SQL.QueryRow("SELECT password_hash FROM users WHERE id='user'").Scan(&hash); err != nil || hash != "existing-password-hash" {
		t.Fatalf("account changed: %s %v", hash, err)
	}
}
func TestReleaseUpdateFromV050(t *testing.T) {
	target := buildTarget(t)
	g := releaseServer(t, target, false)
	for _, role := range []string{"control", "worker", "registry", "share-node"} {
		t.Run(role, func(t *testing.T) {
			dir := t.TempDir()
			o := options{role: role, binDir: dir, repo: "arusuki/alpha"}
			var db *platform.Database
			if role != "share-node" {
				db = oldDatabase(t, role)
				o.directory = db.Directory
				if role == "worker" {
					_, e := db.SQL.Exec(`INSERT INTO managed_containers VALUES('container','unix:///docker.sock','daemon','alpha-member','alice','{}','fingerprint','create','',1,123);
                    INSERT INTO member_container_slots(member_id,username,plan,deleted,mode,container_id) VALUES('member','alice','{}',0,'create','container');`)
					if e != nil {
						t.Fatal(e)
					}
				}
				if role == "control" {
					_, e := db.SQL.Exec(`INSERT INTO members VALUES('member','alice','{}','{}','[{"node_id":"node","mode":"create","container_id":""}]','password-hash','password-ciphertext','existing-public-key','token-hash','active','invite','invite-hash',123);
                    INSERT INTO cluster_nodes VALUES('node','worker','http://127.0.0.1:8766','node-token',123,'worker','10.0.0.1');
                    INSERT INTO member_node_resources VALUES('member','node','ready','container','alpha-member',2222,'',123,'create','');`)
					if e != nil {
						t.Fatal(e)
					}
				}
			}
			for _, f := range binaries(role) {
				if f.source != "alpha-updater" {
					writeFile(t, filepath.Join(dir, f.destination), script(f.source, "v0.5.0"))
				}
			}
			// Helpers not required by a role must never be overwritten.
			if role != "worker" {
				writeFile(t, filepath.Join(dir, "rootless-docker"), []byte("untouched"))
			}
			var out bytes.Buffer
			if err := run(context.Background(), o, g, &out); err != nil {
				t.Fatalf("%v\n%s", err, &out)
			}
			if db != nil {
				assertVersion(t, db, platform.DatabaseVersion)
				if role == "worker" {
					var mode, cid string
					if e := db.SQL.QueryRow("SELECT mode,container_id FROM member_container_slots WHERE member_id='member'").Scan(&mode, &cid); e != nil || mode != "create" || cid != "container" {
						t.Fatal("lost container binding", mode, cid, e)
					}
				}
				if role == "control" {
					var choices, password string
					if e := db.SQL.QueryRow("SELECT registration_containers,password_ciphertext FROM members WHERE id='member'").Scan(&choices, &password); e != nil || !strings.Contains(choices, "node") || password != "password-ciphertext" {
						t.Fatal("lost registration", choices, password, e)
					}
					var mode, state, cid string
					if e := db.SQL.QueryRow("SELECT mode,state,container_id FROM member_node_resources WHERE member_id='member'").Scan(&mode, &state, &cid); e != nil || mode != "create" || state != "ready" || cid != "container" {
						t.Fatal("lost resource", mode, state, cid, e)
					}
				}
				backups, _ := filepath.Glob(filepath.Join(db.Directory, "platform.sqlite3.backup-*"))
				if len(backups) != 1 {
					t.Fatal(backups)
				}
				backup, err := sql.Open("sqlite3", backups[0])
				if err != nil {
					t.Fatal(err)
				}
				defer backup.Close()
				assertVersion(t, &platform.Database{SQL: backup}, 36)
			}
			got, err := executableVersion(context.Background(), filepath.Join(dir, binaries(role)[0].destination), "project-alpha")
			if err != nil || got != "v0.6.0" {
				t.Fatal(got, err)
			}
			if _, err := os.Stat(filepath.Join(dir, ".alpha-update-pending")); !os.IsNotExist(err) {
				t.Fatal("journal retained", err)
			}
			if role != "worker" {
				b, _ := os.ReadFile(filepath.Join(dir, "rootless-docker"))
				if string(b) != "untouched" {
					t.Fatal("wrong role files updated")
				}
			}
		})
	}
	t.Run("migration failure restores binaries", func(t *testing.T) {
		db := oldDatabase(t, "worker")
		dir := t.TempDir()
		if _, err := db.SQL.Exec("INSERT INTO owners VALUES('a','alice'),('b','bob'); ALTER TABLE settings ADD COLUMN schedule_last_run REAL NOT NULL DEFAULT 0"); err != nil {
			t.Fatal(err)
		}
		for _, f := range binaries("worker") {
			writeFile(t, filepath.Join(dir, f.destination), script(f.source, "v0.5.0"))
		}
		err := run(context.Background(), options{role: "worker", directory: db.Directory, binDir: dir}, g, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "previous binaries restored") {
			t.Fatal(err)
		}
		assertVersion(t, db, 36)
		for _, f := range binaries("worker") {
			b, _ := os.ReadFile(filepath.Join(dir, f.destination))
			if !bytes.Equal(b, script(f.source, "v0.5.0")) {
				t.Fatal("binary not restored", f)
			}
		}
		var count int
		if err := db.SQL.QueryRow("SELECT count(*) FROM owners").Scan(&count); err != nil || count != 2 {
			t.Fatal("lost ownership", count, err)
		}
	})
}

func TestRejectedUpdatesPreserveFiles(t *testing.T) {
	for _, scenario := range []string{"check", "running", "wrong-role", "checksum", "pending", "unsupported"} {
		t.Run(scenario, func(t *testing.T) {
			db := oldDatabase(t, "worker")
			dir := t.TempDir()
			original := script("project-alpha", "v0.5.0")
			writeFile(t, filepath.Join(dir, "project-alpha"), original)
			o := options{role: "auto", directory: db.Directory, binDir: dir}
			switch scenario {
			case "check":
				o.check = true
			case "running":
				lock, err := db.LockService()
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			case "wrong-role":
				o.role = "control"
			case "unsupported":
				if _, err := db.SQL.Exec("PRAGMA user_version=35"); err != nil {
					t.Fatal(err)
				}
			case "pending":
				writeFile(t, filepath.Join(dir, ".alpha-update-pending"), []byte("interrupted"))
			}
			g := releaseServer(t, script("alpha-updater", "v0.6.0"), scenario == "checksum")
			err := run(context.Background(), o, g, io.Discard)
			if scenario == "check" {
				if err != nil {
					t.Fatal(err)
				}
				entries, _ := os.ReadDir(dir)
				if len(entries) != 1 {
					t.Fatal("check changed directory", entries)
				}
			} else if err == nil {
				t.Fatal("expected rejection")
			}
			wantVersion := 36
			if scenario == "unsupported" {
				wantVersion = 35
				if err == nil || !strings.Contains(err.Error(), "unsupported database version") {
					t.Fatalf("expected explicit unsupported version error: %v", err)
				}
				entries, readErr := os.ReadDir(dir)
				if readErr != nil || len(entries) != 1 {
					t.Fatalf("unsupported version changed installation: %v %v", entries, readErr)
				}
			}
			assertVersion(t, db, wantVersion)
			got, _ := os.ReadFile(filepath.Join(dir, "project-alpha"))
			if !bytes.Equal(got, original) {
				t.Fatal("binary changed")
			}
		})
	}
}

func TestInstallRollbackAndUncertainCommit(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint(uncertain), func(t *testing.T) {
			dir := t.TempDir()
			stage := t.TempDir()
			writeFile(t, filepath.Join(dir, "project-alpha"), []byte("old"))
			writeFile(t, filepath.Join(stage, "project-alpha"), []byte("new"))
			writeFile(t, filepath.Join(stage, "alpha-updater"), []byte("new updater"))
			failure := errors.New("failed transaction")
			if uncertain {
				failure = errMigrationUncertain
			}
			err := install(dir, stage, binaries("control"), "backup", func() error { return failure }, io.Discard)
			if err == nil {
				t.Fatal("missing error")
			}
			b, _ := os.ReadFile(filepath.Join(dir, "project-alpha"))
			_, journalErr := os.Stat(filepath.Join(dir, ".alpha-update-pending"))
			if uncertain {
				if string(b) != "new" || journalErr != nil {
					t.Fatal("lost recovery state")
				}
			} else {
				if string(b) != "old" || !os.IsNotExist(journalErr) {
					t.Fatal("did not restore")
				}
				if _, err := os.Stat(filepath.Join(dir, "alpha-updater")); !os.IsNotExist(err) {
					t.Fatal("new file not removed")
				}
			}
		})
	}
}

func TestArchiveAndReleaseValidation(t *testing.T) {
	for _, entry := range []string{"../escape", "/absolute", "package/bin/alpha-updater"} {
		data := makeArchive(t, map[string][]byte{entry: []byte("x")})
		if err := extract(bytes.NewReader(data), t.TempDir(), "package", []string{"project-alpha"}); err == nil {
			t.Fatal("accepted", entry)
		}
	}
	if _, err := checksum(strings.Repeat("0", 64)+"  package\n"+strings.Repeat("0", 64)+"  package", "package"); err == nil {
		t.Fatal("duplicate checksum accepted")
	}
	ordered := []string{"v0.3.1", "v0.3.2-alpha.2", "v0.3.2-alpha.10", "v0.3.2-beta", "v0.3.2", "v0.10.0"}
	for i := 1; i < len(ordered); i++ {
		a, _ := parseVersion(ordered[i-1])
		b, _ := parseVersion(ordered[i])
		if a.compare(b) >= 0 {
			t.Fatal(ordered[i])
		}
	}
	for _, tag := range []string{"v0.3.0", "v1.0.0", "v0.3.2-01", "../../escape", "dev"} {
		if _, err := supportedVersion(tag); err == nil {
			t.Fatal("accepted", tag)
		}
	}
}

func TestPrereleaseSelectionAndToken(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing auth")
		}
		if r.URL.Query().Get("page") != "1" {
			t.Error(r.URL)
		}
		json.NewEncoder(w).Encode([]release{
			{Tag: "v0.3.2", Published: time.Unix(100, 0)},
			{Tag: "v0.3.3-rc.1", Prerelease: true, Published: time.Unix(200, 0)},
			{Tag: "v0.3.3", Draft: true, Published: time.Unix(300, 0)},
		})
	}))
	defer server.Close()
	g := github{server.Client(), server.URL, "arusuki/alpha", "test-token"}
	got, err := g.latest(context.Background(), true)
	if err != nil || got.Tag != "v0.3.3-rc.1" {
		t.Fatal(got, err)
	}
	if err := g.get(context.Background(), "https://untrusted.example/asset", "", io.Discard, 10); err == nil {
		t.Fatal("token could leak to foreign API origin")
	}
}

func TestOfflineMigrationGuardsAndIdempotency(t *testing.T) {
	db := oldDatabase(t, "registry")
	if err := Run(context.Background(), []string{"--database-only", "--data-dir", db.Directory}, io.Discard); err != nil {
		t.Fatal(err)
	}
	assertVersion(t, db, platform.DatabaseVersion)
	if err := Run(context.Background(), []string{"--database-only", "--data-dir", db.Directory}, io.Discard); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(filepath.Join(db.Directory, "platform.sqlite3.backup-*"))
	if len(backups) != 1 {
		t.Fatal("idempotent upgrade created extra backups", backups)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if err := Run(context.Background(), []string{"--database-only", "--data-dir", missing}, io.Discard); err == nil {
		t.Fatal("created missing database")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("created missing directory")
	}
	if _, err := db.SQL.Exec("PRAGMA user_version=32"); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), []string{"--database-only", "--data-dir", db.Directory}, io.Discard); err == nil {
		t.Fatal("accepted unsupported schema")
	}
	assertVersion(t, db, 32)
}

func TestPartialInstallFailureRestoresPriorFiles(t *testing.T) {
	dir, stage := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(dir, "project-alpha"), []byte("old"))
	writeFile(t, filepath.Join(stage, "project-alpha"), []byte("new"))
	called := false
	err := install(dir, stage, binaries("control"), "backup", func() error { called = true; return nil }, io.Discard)
	if err == nil || called {
		t.Fatal("missing staged updater did not abort before migration", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "project-alpha"))
	if string(b) != "old" {
		t.Fatal("partial binary update not rolled back")
	}
}
