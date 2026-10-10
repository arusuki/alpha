package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSameVersionWorkerPreparesAndUpdatesServices(t *testing.T) {
	f, db, _ := newDockerFixture(t)
	dir := t.TempDir()
	for _, file := range binaries("worker") {
		writeFile(t, filepath.Join(dir, file.destination), script(file.source, "v0.6.0"))
	}
	runDocker := func(ctx context.Context, endpoint string, args []string, input []byte) ([]byte, error) {
		if args[0] == "build" {
			return nil, nil
		}
		return f.run(ctx, endpoint, args, input)
	}
	var prepared *PreparedUpdate
	o := options{role: "worker", directory: db.Directory, binDir: dir, docker: runDocker, prepare: func(p *PreparedUpdate) error { prepared = p; return nil }}
	lock, err := db.LockService()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := run(context.Background(), o, releaseServer(t, script("alpha-updater", "v0.6.0"), false), io.Discard); err != nil {
		t.Fatal(err)
	}
	if prepared == nil || len(prepared.Containers.Updates) != 1 {
		t.Fatal("same-version worker skipped services", prepared)
	}
	if f.container["Image"] != f.old {
		t.Fatal("online preparation replaced container")
	}
	lock.Close()
	o.prepare = nil
	o.prepared = prepared
	if err := runConfigured(context.Background(), o, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.container["Image"] != f.next {
		t.Fatal("service image unchanged")
	}
	assertVersion(t, db, 37)
	for _, file := range binaries("worker") {
		raw, err := os.ReadFile(filepath.Join(dir, file.destination))
		if err != nil || !bytes.Equal(raw, script(file.source, "v0.6.0")) {
			t.Fatal("same-version sync changed binary", err)
		}
	}
	if attempted, err := ServicesAttempted(db.Directory, "v0.6.0"); err != nil || !attempted {
		t.Fatal("missing sync receipt", err)
	}
	backups, _ := filepath.Glob(filepath.Join(db.Directory, "platform.sqlite3.backup-*"))
	if len(backups) != 0 {
		t.Fatal("same-version synchronization touched database")
	}
}

func TestUnchangedServiceImageAndConfigSkipRecreation(t *testing.T) {
	f, db, stage := newDockerFixture(t)
	p, err := prepareContainers(context.Background(), db, stage, f.run, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(p.Updates[0].After, &config); err != nil {
		t.Fatal(err)
	}
	f.compose = config
	f.container["Image"] = f.next
	spec := config["services"].(map[string]any)["dram-bw"].(map[string]any)
	f.container["Config"].(map[string]any)["Entrypoint"] = spec["entrypoint"]
	p, err = prepareContainers(context.Background(), db, stage, f.run, io.Discard)
	if err != nil || len(p.Updates) != 0 {
		t.Fatal("unchanged service scheduled for replacement", p, err)
	}
}

func TestServiceSyncReceiptValidation(t *testing.T) {
	dir := t.TempDir()
	if done, err := ServicesAttempted(dir, "v0.8.0-rc3"); err != nil || done {
		t.Fatal(done, err)
	}
	if err := MarkServicesAttempted(dir, "v0.8.0-rc3"); err != nil {
		t.Fatal(err)
	}
	if done, err := ServicesAttempted(dir, "v0.8.0-rc3"); err != nil || !done {
		t.Fatal(done, err)
	}
	if done, err := ServicesAttempted(dir, "v0.8.0-rc4"); err != nil || done {
		t.Fatal(done, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "update-services.json"), []byte(`{"tag":"v0.8.0-rc3","state":"unknown"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ServicesAttempted(dir, "v0.8.0-rc3"); err == nil {
		t.Fatal("invalid receipt accepted")
	}
}
