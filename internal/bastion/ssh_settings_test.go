package bastion

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"project-alpha/internal/members"
	"project-alpha/internal/platform"
	"project-alpha/internal/testutil"
)

func sshSettingsFixture(t *testing.T) (*Handler, *testutil.Client) {
	t.Helper()
	db, err := platform.OpenDatabase(t.TempDir(), func(tx *sql.Tx) error {
		if err := members.Initialize(tx); err != nil {
			return err
		}
		return Initialize(tx)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	h := NewHandler(db)
	client := &testutil.Client{T: t, Handler: platform.NewServer(db, h, nil, nil, false)}
	return h, client
}

// Mock the executables with inert file contents; never generate test keys.
func mockIdentityCommands(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	keygen := `#!/bin/sh
derive=false
path=''
for arg do
    if [ "$arg" = '-y' ]; then derive=true; fi
    path="$arg"
done
if [ "$ALPHA_SSH_KEYGEN_FAIL" = 'yes' ]; then exit 1; fi
if [ "$derive" = 'false' ]; then
    umask 077
    printf '%s\n' 'inert-private-placeholder' > "$path"
    printf '%s\n' '` + testKey + `' > "$path.pub"
fi
printf '%s\n' '` + testKey + `'
`
	ssh := `#!/bin/sh
printf '%s\n' "$*" >> "$ALPHA_SSH_ARGS_LOG"
if [ "$ALPHA_SSH_FAIL" = 'yes' ]; then exit 1; fi
printf '%s\n' '{"version":` + strconv.Itoa(keyFormat) + `,"listen_host":"100.64.0.2","status_port":8765,"control_url":"http://10.0.0.1:8765","keys":{}}'
`
	for name, body := range map[string]string{"ssh-keygen": keygen, "ssh": ssh} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ALPHA_SSH_ARGS_LOG", filepath.Join(directory, "args.log"))
	return directory
}

func TestSSHSettingsAccessValidationAndPublicKey(t *testing.T) {
	h, client := sshSettingsFixture(t)
	mockIdentityCommands(t)
	client.Expect(401, "GET", "/api/bastion/ssh", nil, nil)
	client.Expect(401, "GET", "/api/bastion/ssh/public-key", nil, nil)
	client.Expect(401, "POST", "/api/bastion/ssh/generate", map[string]any{"name": "control"}, nil)
	client.Login(true, "operator", "A-test-password-123")
	cfg := client.Expect(200, "GET", "/api/bastion/ssh", nil, nil)
	if cfg["identity_file"] != "" || cfg["revision"] != float64(1) {
		t.Fatal(cfg)
	}
	client.Expect(403, "POST", "/api/bastion/ssh/generate", map[string]any{"name": "control"}, map[string]string{"X-CSRF-Token": "wrong"})
	for _, value := range []map[string]any{
		{"revision": 1}, {"revision": 1, "identity_file": nil},
		{"revision": 1, "identity_file": "relative"}, {"revision": 1, "identity_file": "/tmp/key\nname"},
		{"revision": 1, "identity_file": "/missing-key"}, {"revision": 1, "identity_file": "", "private_key": "secret"},
	} {
		client.Expect(400, "PUT", "/api/bastion/ssh", value, nil)
	}
	external := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(external, []byte("inert-placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", external} {
		client.Expect(400, "PUT", "/api/bastion/ssh", map[string]any{"revision": 1, "identity_file": path}, nil)
		client.Expect(400, "GET", "/api/bastion/ssh/public-key?identity_file="+url.QueryEscape(path), nil, nil)
	}
	if slices.Contains(h.availableIdentities(), external) {
		t.Fatal("external identity listed")
	}
	path := client.Expect(201, "POST", "/api/bastion/ssh/generate", map[string]any{"name": "control"}, nil)["identity_file"].(string)
	if err := os.WriteFile(path, []byte("inert-private-placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	// A stale companion public key must never be returned as this identity.
	if err := os.WriteFile(path+".pub", []byte("stale public key"), 0644); err != nil {
		t.Fatal(err)
	}
	public := client.Expect(200, "GET", "/api/bastion/ssh/public-key?identity_file="+url.QueryEscape(path), nil, nil)
	if public["public_key"] != testKey || !strings.HasPrefix(public["fingerprint"].(string), "SHA256:") || strings.Contains(public["public_key"].(string), "private") {
		t.Fatal(public)
	}
	client.Expect(200, "PUT", "/api/bastion/ssh", map[string]any{"revision": 1, "identity_file": path}, nil)
	client.Expect(409, "PUT", "/api/bastion/ssh", map[string]any{"revision": 1, "identity_file": ""}, nil)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	client.Expect(400, "GET", "/api/bastion/ssh/public-key?identity_file="+url.QueryEscape(path), nil, nil)
	broken := client.Expect(200, "GET", "/api/bastion/ssh", nil, nil)
	if broken["error"] == "" || broken["public_key"] != "" || broken["identity_file"] != path {
		t.Fatal("invalid identity was hidden or replaced", broken)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	// Reopening the same database preserves the current selection.
	reopened, err := platform.OpenDatabase(h.DB.Directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.SQL.Close()
	saved, err := NewHandler(reopened).sshSettings()
	if err != nil || saved.IdentityFile != path || saved.Revision != 2 {
		t.Fatalf("%+v %v", saved, err)
	}
	client.Expect(400, "PUT", "/api/bastion/ssh", map[string]any{"revision": 2, "identity_file": ""}, nil)
	client.Expect(201, "POST", "/api/users", map[string]any{"username": "reader", "password": "A-test-password-456", "role": "viewer"}, nil)
	client.Login(false, "reader", "A-test-password-456")
	for _, route := range []string{"/api/bastion/ssh", "/api/bastion/ssh/public-key?identity_file=" + url.QueryEscape(path)} {
		client.Expect(403, "GET", route, nil, nil)
	}
	client.Expect(403, "POST", "/api/bastion/ssh/generate", map[string]any{"name": "control"}, nil)
}

func TestSSHGenerationPreservesIdentityAndExistingFiles(t *testing.T) {
	h, client := sshSettingsFixture(t)
	mockIdentityCommands(t)
	client.Login(true, "operator", "A-test-password-123")
	for _, name := range []string{"", "../escape", "-option", "control name"} {
		client.Expect(400, "POST", "/api/bastion/ssh/generate", map[string]any{"name": name}, nil)
	}
	first := client.Expect(201, "POST", "/api/bastion/ssh/generate", map[string]any{"name": "control"}, nil)
	second := client.Expect(201, "POST", "/api/bastion/ssh/generate", map[string]any{"name": "control"}, nil)
	if first["identity_file"] == second["identity_file"] || first["public_key"] != testKey {
		t.Fatal(first, second)
	}
	path := first["identity_file"].(string)
	for file, mode := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700, h.identityDirectory(): 0700} {
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("unsafe generated file %s: %v %v", file, info, err)
		}
	}
	cfg := client.Expect(200, "GET", "/api/bastion/ssh", nil, nil)
	if cfg["identity_file"] != "" || cfg["revision"] != float64(1) || len(cfg["identities"].([]any)) < 2 {
		t.Fatal("generation activated an identity or lost generated choices", cfg)
	}
	// A new handler on the reopened database must discover both inactive keys.
	reopened, err := platform.OpenDatabase(h.DB.Directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.SQL.Close()
	restarted := NewHandler(reopened)
	saved, err := restarted.sshSettings()
	if err != nil || saved.IdentityFile != "" {
		t.Fatalf("generation changed the persisted active identity: %+v %v", saved, err)
	}
	choices := restarted.availableIdentities()
	for _, generated := range []map[string]any{first, second} {
		path := generated["identity_file"].(string)
		if !slices.Contains(choices, path) {
			t.Fatalf("generated key missing after restart: %s in %v", path, choices)
		}
		identity, err := inspectIdentity(context.Background(), path)
		if err != nil || identity.PublicKey != generated["public_key"] {
			t.Fatalf("cannot recover generated public key after restart: %+v %v", identity, err)
		}
	}
	t.Setenv("ALPHA_SSH_KEYGEN_FAIL", "yes")
	client.Expect(500, "POST", "/api/bastion/ssh/generate", map[string]any{"name": "failed"}, nil)
	entries, err := os.ReadDir(h.identityDirectory())
	if err != nil || len(entries) != 2 {
		t.Fatalf("failed generation changed existing directories: %v %v", entries, err)
	}
	// Reject a linked storage directory without touching its target.
	h.DB.Directory = t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, h.identityDirectory()); err != nil {
		t.Fatal(err)
	}
	client.Expect(400, "POST", "/api/bastion/ssh/generate", map[string]any{"name": "linked"}, nil)
	entries, err = os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatal("modified symlink target")
	}
}

func TestSSHIdentitySwitchChecksDisabledNodesAndKeepsOldSettings(t *testing.T) {
	h, client := sshSettingsFixture(t)
	directory := mockIdentityCommands(t)
	client.Login(true, "operator", "A-test-password-123")
	identity := client.Expect(201, "POST", "/api/bastion/ssh/generate", map[string]any{"name": "control"}, nil)
	path := identity["identity_file"].(string)
	if _, err := h.DB.SQL.Exec("INSERT INTO bastion_tailscale VALUES('disabled','Disabled node',0,'100.64.0.2',22,8765,'http://10.0.0.1:8765')"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ALPHA_SSH_FAIL", "yes")
	client.Expect(409, "PUT", "/api/bastion/ssh", map[string]any{"revision": 1, "identity_file": path}, nil)
	cfg, err := h.sshSettings()
	if err != nil || cfg.IdentityFile != "" || cfg.Revision != 1 {
		t.Fatal("failed switch changed settings", cfg, err)
	}
	t.Setenv("ALPHA_SSH_FAIL", "no")
	client.Expect(200, "PUT", "/api/bastion/ssh", map[string]any{"revision": 1, "identity_file": path}, nil)
	node, err := h.share("disabled")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.sshCommand(context.Background(), node, commandRequest{Operation: "inspect"}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(filepath.Join(directory, "args.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range strings.Split(strings.TrimSpace(string(args)), "\n") {
		for _, want := range []string{"-F /dev/null", "-i " + path, "IdentitiesOnly=yes", "StrictHostKeyChecking=accept-new", "-l alpha-worker", "alpha-worker cmd"} {
			if !strings.Contains(call, want) {
				t.Fatalf("missing %q in %s", want, call)
			}
		}
	}
}

func TestSSHIdentitySupportsTransactionalKeyManagement(t *testing.T) {
	h, _, m := fixture(t)
	mockIdentityCommands(t)
	identity, err := h.generateIdentity(context.Background(), "control", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.saveSSHSettings(context.Background(), sshSettings{Revision: 1, IdentityFile: identity.IdentityFile}, "operator"); err != nil {
		t.Fatal(err)
	}
	// Both operations hold the sole database connection while invoking SSH.
	if err = h.SyncKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = h.editMemberKey(context.Background(), m.ID, ""); err != nil {
		t.Fatal(err)
	}
}

func TestSSHIdentityDeletion(t *testing.T) {
	h, client := sshSettingsFixture(t)
	mockIdentityCommands(t)
	endpoint := "/api/bastion/ssh/identity"
	client.Expect(401, "DELETE", endpoint, map[string]any{"identity_file": "/missing"}, nil)
	client.Login(true, "operator", "A-test-password-123")
	generate := func(name string) string {
		return client.Expect(201, "POST", "/api/bastion/ssh/generate", map[string]any{"name": name}, nil)["identity_file"].(string)
	}
	active, spare := generate("active"), generate("spare")
	client.Expect(200, "PUT", "/api/bastion/ssh", map[string]any{"revision": 1, "identity_file": active}, nil)
	client.Expect(403, "DELETE", endpoint, map[string]any{"identity_file": spare}, map[string]string{"X-CSRF-Token": "wrong"})
	client.Expect(409, "DELETE", endpoint, map[string]any{"identity_file": active}, nil)
	for _, path := range []string{"", "relative", filepath.Dir(spare), spare + ".pub", filepath.Join(h.identityDirectory(), "..", "outside", "id_ed25519")} {
		client.Expect(400, "DELETE", endpoint, map[string]any{"identity_file": path}, nil)
	}
	outside := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(outside, []byte("external-placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	client.Expect(400, "DELETE", endpoint, map[string]any{"identity_file": outside}, nil)
	linkedDir := filepath.Join(h.identityDirectory(), "linked")
	if err := os.Symlink(filepath.Dir(outside), linkedDir); err != nil {
		t.Fatal(err)
	}
	client.Expect(400, "DELETE", endpoint, map[string]any{"identity_file": filepath.Join(linkedDir, "id_ed25519")}, nil)
	linkedFileDir := filepath.Join(h.identityDirectory(), "linked-file")
	if err := os.Mkdir(linkedFileDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(linkedFileDir, "id_ed25519")); err != nil {
		t.Fatal(err)
	}
	client.Expect(400, "DELETE", endpoint, map[string]any{"identity_file": filepath.Join(linkedFileDir, "id_ed25519")}, nil)
	// Aliases cannot bypass selection from the managed key directory.
	alias := filepath.Join(t.TempDir(), "active-alias")
	if err := os.Symlink(active, alias); err != nil {
		t.Fatal(err)
	}
	client.Expect(400, "PUT", "/api/bastion/ssh", map[string]any{"revision": 2, "identity_file": alias}, nil)
	client.Expect(409, "DELETE", endpoint, map[string]any{"identity_file": active}, nil)
	extra := filepath.Join(filepath.Dir(spare), "keep.txt")
	if err := os.WriteFile(extra, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	client.Expect(400, "DELETE", endpoint, map[string]any{"identity_file": spare}, nil)
	for _, path := range []string{spare, spare + ".pub", active, active + ".pub", outside, extra} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("rejected deletion changed %s: %v", path, err)
		}
	}
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	client.Expect(200, "DELETE", endpoint, map[string]any{"identity_file": spare}, nil)
	for _, path := range []string{spare, spare + ".pub", filepath.Dir(spare)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("deleted identity remains: %s: %v", path, err)
		}
	}
	client.Expect(404, "DELETE", endpoint, map[string]any{"identity_file": spare}, nil)
	cfg := client.Expect(200, "GET", "/api/bastion/ssh", nil, nil)
	if cfg["identity_file"] != active || cfg["revision"] != float64(2) {
		t.Fatal("deletion changed active identity", cfg)
	}
	if slices.Contains(NewHandler(h.DB).availableIdentities(), spare) {
		t.Fatal("deleted identity returned after reload")
	}
	var audits int
	if err := h.DB.SQL.QueryRow("SELECT count(*) FROM audit WHERE actor='operator' AND action='bastion.ssh.delete' AND detail=?", spare).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("missing deletion audit: %d %v", audits, err)
	}
	replacement := generate("replacement")
	client.Expect(200, "PUT", "/api/bastion/ssh", map[string]any{"revision": 2, "identity_file": replacement}, nil)
	if err := os.Remove(active + ".pub"); err != nil {
		t.Fatal(err)
	}
	// Missing companion public files do not prevent removing the private file.
	client.Expect(200, "DELETE", endpoint, map[string]any{"identity_file": active}, nil)
	if content, err := os.ReadFile(outside); err != nil || string(content) != "external-placeholder" {
		t.Fatalf("external identity changed: %q %v", content, err)
	}
	client.Expect(201, "POST", "/api/users", map[string]any{"username": "reader", "password": "A-test-password-456", "role": "viewer"}, nil)
	client.Login(false, "reader", "A-test-password-456")
	client.Expect(403, "DELETE", endpoint, map[string]any{"identity_file": spare}, nil)
}

func TestSSHRequiresManagedIdentityBeforeConnecting(t *testing.T) {
	h, _ := sshSettingsFixture(t)
	directory := mockIdentityCommands(t)
	_, err := h.sshCommand(context.Background(), ShareNode{ID: "node", Name: "Node", SSHHost: "100.64.0.2", SSHPort: 22, StatusPort: 8765, ControlURL: "http://10.0.0.1:8765"}, commandRequest{Operation: "inspect"})
	if err == nil {
		t.Fatal("connection without a selected identity succeeded")
	}
	if _, err := os.Stat(filepath.Join(directory, "args.log")); !os.IsNotExist(err) {
		t.Fatal("SSH ran before identity selection")
	}
}

func TestSSHConfigDiscoveryPreservesNormalDriftChecks(t *testing.T) {
	h, client := sshSettingsFixture(t)
	mockIdentityCommands(t)
	client.Login(true, "operator", "A-test-password-123")
	identity := client.Expect(201, "POST", "/api/bastion/ssh/generate", map[string]any{"name": "control"}, nil)["identity_file"].(string)
	s := ShareNode{SSHHost: "100.64.0.2", SSHPort: 22}
	req := commandRequest{Operation: "inspect", DiscoverConfig: true}
	for _, stored := range []bool{false, true} {
		if stored {
			s.StatusPort = 9765
			s.ControlURL = "http://10.0.0.9:8765"
		}
		reply, err := h.sshCommandWithIdentity(context.Background(), s, req, identity)
		if err != nil || reply.StatusPort != 8765 || reply.ControlURL != "http://10.0.0.1:8765" {
			t.Fatalf("discovery failed: %+v %v", reply, err)
		}
		if _, err := h.sshCommandWithIdentity(context.Background(), s, commandRequest{Operation: "inspect"}, identity); err == nil {
			t.Fatal("normal inspection accepted missing or changed configuration")
		}
	}
	// Discovery cannot bypass saved configuration checks for key mutations.
	req.Operation = "ensure"
	if _, err := h.sshCommandWithIdentity(context.Background(), s, req, identity); err == nil {
		t.Fatal("key mutation accepted discovery mode")
	}
}
