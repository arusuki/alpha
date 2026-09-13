package storage

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func TestDirectoryUpdatesPublishBeforeCompletionAndResume(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	target := filepath.Join(p.storage, "data")
	for i, name := range []string{"MNIST-v2", "other"} {
		dir := filepath.Join(target, name, "nested")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(dir, "file.bin"), make([]byte, (i+1)*20*1024))
	}
	c := defaultConfig()
	c.NoDocker, c.Root, c.MaxDepth = true, []string{p.storage}, 1
	base, err := buildSnapshot(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, worker := platform.RandomHex(16), platform.RandomHex(16)
	base.JobID = id
	plan := scanPlan{Config: c, BaseJobID: id, IncrementalPath: target}
	if _, err := p.db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config) VALUES(?,'completed','manual','test',1,?),(?,'running','incremental','test',2,?)", id, httpapi.JSONText(c), worker, httpapi.JSONText(plan)); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(filepath.Join(p.db.Directory, "results", id, "snapshot.json"), base); err != nil {
		t.Fatal(err)
	}
	before := p.expect(200, "GET", "/api/jobs/"+id+"/snapshot", nil, nil)
	var observations []*Snapshot
	final, err := expandDirectory(context.Background(), base, c, target, nil, func(next *Snapshot) error {
		if err := p.db.publishDirectory(context.Background(), worker, plan, next, object{}, false); err != nil {
			return err
		}
		job, err := p.db.job(worker)
		if err != nil {
			return err
		}
		if job["status"] != "running" {
			return fmt.Errorf("checkpoint completed the worker")
		}
		stored, err := p.db.readSnapshot(id)
		if err != nil {
			return err
		}
		n := snapshotNodes(stored.Tree)[target]
		if !n.Scanning || len(n.Children) == 0 || stored.Revision != next.Revision {
			return fmt.Errorf("observation was not persisted during traversal")
		}
		changes := p.expect(200, "GET", fmt.Sprintf("/api/jobs/%s/changes?revision=%d", id, next.Revision-1), nil, nil)
		current := p.expect(200, "GET", "/api/jobs/"+id+"/snapshot", nil, nil)
		assertChangesReconstruct(t, before, changes, current)
		before = current
		observations = append(observations, stored)
		// Force another measurable checkpoint on this tiny fixture.
		time.Sleep(510 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) < 2 {
		t.Fatal("no intermediate directory updates")
	}
	first := snapshotNodes(observations[0].Tree)[target]
	last := snapshotNodes(observations[len(observations)-1].Tree)[target]
	residual := func(n *Node) int64 {
		bytes := n.Allocated
		for _, child := range n.Children {
			bytes -= child.Allocated
		}
		return bytes
	}
	if residual(first) <= residual(last) || len(first.Children) >= len(last.Children) {
		t.Fatal("gray historical block did not split progressively")
	}
	if err := p.db.publishDirectory(context.Background(), worker, plan, final, object{}, true); err != nil {
		t.Fatal(err)
	}
	stored, err := p.db.readSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	n := snapshotNodes(stored.Tree)[target]
	if n.Scanning || len(n.Children) != 2 || stored.Tree.Allocated != base.Tree.Allocated {
		t.Fatal("final measurement did not replace historical bytes exactly")
	}
	for _, child := range n.Children {
		if child.SizeUnknown || child.Allocated <= 20*1024 || child.Omitted == 0 {
			t.Fatal("next-layer directory has no measured size", child)
		}
	}
	// Resume the real SSE endpoint from an earlier revision; only new changes
	// arrive, and the wire payload reconstructs the final saved record.
	server := httptest.NewServer(p.s)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/jobs/"+id+"/events?revision=0", nil)
	req.Header.Set("Cookie", p.cookie)
	req.Header.Set("Last-Event-ID", fmt.Sprint(observations[0].Revision))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatal("SSE unavailable", response.Status)
	}
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			var patch object
			if err := json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &patch); err != nil {
				t.Fatal(err)
			}
			if numberInt64(patch["base_revision"]) != observations[0].Revision || numberInt64(patch["revision"]) != final.Revision {
				t.Fatal("SSE resume lost its cursor", patch)
			}
			cancel()
			return
		}
	}
	t.Fatal("no SSE changes", scanner.Err())
}

func TestDirectoryCancellationRetainsPublishedObservation(t *testing.T) {
	p := newTestPlatform(t)
	base, c, target, _ := incrementalFixture(t)
	id, worker := platform.RandomHex(16), platform.RandomHex(16)
	plan := scanPlan{Config: c, BaseJobID: id, IncrementalPath: target}
	if _, err := p.db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config) VALUES(?,'completed','manual','test',1,?),(?,'running','incremental','test',2,?)", id, httpapi.JSONText(c), worker, httpapi.JSONText(plan)); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(filepath.Join(p.db.Directory, "results", id, "snapshot.json"), base); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := expandDirectory(ctx, base, c, target, nil, func(next *Snapshot) error {
		if err := p.db.publishDirectory(ctx, worker, plan, next, object{}, false); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	})
	if err != context.Canceled {
		t.Fatal("cancellation ignored", err)
	}
	stored, err := p.db.readSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != 1 || !snapshotNodes(stored.Tree)[target].Scanning {
		t.Fatal("cancel discarded committed observations")
	}
	if _, err := p.db.SQL.Exec("UPDATE jobs SET status='cancelled' WHERE id=?", worker); err != nil {
		t.Fatal(err)
	}
	if _, err := p.api.snapshotChanges(id, 0); err != nil {
		t.Fatal("cancelled worker broke published revision history", err)
	}
}
