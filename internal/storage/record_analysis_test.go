package storage

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDetailScanMetadataAndGuard(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "old.parquet")
	mustWrite(t, file, make([]byte, 8192))
	old := time.Now().Add(-200 * 24 * time.Hour)
	os.Chtimes(file, old, old)
	os.Link(file, filepath.Join(dir, "alias.parquet"))
	mustWrite(t, filepath.Join(dir, "new.jsonl"), make([]byte, 16384))
	c := defaultConfig()
	c.NoDocker = true
	c.Root = []string{dir}
	c.MaxDepth = 0
	c.MaxNodes = 100
	s, err := InspectDirectories(context.Background(), c, DirectoryInspection{Paths: []string{dir}, AnalyzeFiles: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Analysis == nil || len(s.Analysis.Largest) != 2 || s.Analysis.Modified[2].Files != 1 || s.Analysis.Largest[0].Apparent != 16384 {
		t.Fatalf("incorrect metadata: %+v", s.Analysis)
	}
	if len(s.Analysis.Types) != 2 || s.Analysis.Modified[0].Files != 1 {
		t.Fatal("hardlink counted twice or age wrong")
	}
	c.Exclude = []string{filepath.Join(dir, "excluded")}
	os.MkdirAll(c.Exclude[0], 0700)
	os.Symlink(c.Exclude[0], filepath.Join(dir, "escape"))
	for _, path := range []string{"relative", "/proc/1", "/sys", "/dev", "/run", "/var/lib/docker/overlay2/layer/merged", filepath.Join(dir, "escape")} {
		if _, err := validateDetailPath(path, c, ""); err == nil {
			t.Fatalf("accepted forbidden path %s", path)
		}
	}
	c = fullScanConfig(defaultConfig())
	if !c.IncludeDockerRoot || !containsString(c.Root, "/") {
		t.Fatal("production baseline is not full disk")
	}
}
func containsString(a []string, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}

func TestAgentUsageMatchesFrontend(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	raw, err := os.ReadFile("../../tests/fixtures/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	if err = json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	u := buildUsage(&s)
	code := `const U=require('./dist/usage.js'),s=require('./tests/fixtures/snapshot.json'),u=U.build(s);console.log(JSON.stringify({exclusive:u.exclusive,shared:u.shared,crossOwner:u.crossOwner,unrelated:u.unrelated,containers:[...u.containers].map(([id,r])=>({id,exclusive:r.exclusive,shared:r.shared,known:r.known,partial:r.partial}))}));`
	command := exec.Command("node", "-e", code)
	command.Dir = "../.."
	out, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var js struct {
		Exclusive, Shared, CrossOwner, Unrelated int64
		Containers                               []struct {
			ID                string
			Exclusive, Shared int64
			Known, Partial    bool
		}
	}
	if err = json.Unmarshal(out, &js); err != nil {
		t.Fatal(err)
	}
	if u.Exclusive != js.Exclusive || u.Shared != js.Shared || u.CrossOwner != js.CrossOwner || u.Unrelated != js.Unrelated {
		t.Fatalf("accounting differs: %+v / %s", u, out)
	}
	for _, expected := range js.Containers {
		r := u.Containers[expected.ID]
		if r == nil || r.Exclusive != expected.Exclusive || r.Shared != expected.Shared || r.Known != expected.Known || r.Partial != expected.Partial {
			t.Fatalf("container differs: %+v / %+v", r, expected)
		}
	}
}
