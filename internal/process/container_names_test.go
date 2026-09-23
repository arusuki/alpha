package process

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tetragon "github.com/cilium/tetragon/api/v1/tetragon"
)

func TestContainerNamesAndRefresh(t *testing.T) {
	full := strings.Repeat("a", 64)
	calls := 0
	values := map[string]string{full: "training-worker"}
	var readErr error
	names := &containerNames{read: func(context.Context) (map[string]string, error) {
		calls++
		return values, readErr
	}}
	groups := []*Container{{ID: full}, {ID: full[:15]}, {ID: "docker://" + full}, {ID: "unknown", Name: "from-tetragon"}}
	names.apply(context.Background(), groups)
	for _, group := range groups[:3] {
		if group.Name != "training-worker" {
			t.Fatalf("name missing for %s: %+v", group.ID, group)
		}
	}
	if groups[3].Name != "from-tetragon" {
		t.Fatal("lost Tetragon name")
	}
	names.apply(context.Background(), groups)
	if calls != 1 {
		t.Fatal("polling bypassed name cache")
	}
	values = map[string]string{full: "renamed-worker", strings.Repeat("a", 15) + strings.Repeat("b", 49): "other"}
	names.next = time.Time{}
	groups = []*Container{{ID: full}, {ID: full[:15]}}
	names.apply(context.Background(), groups)
	if groups[0].Name != "renamed-worker" || groups[1].Name != "" {
		t.Fatalf("rename or ambiguous prefix handled incorrectly: %+v %+v", groups[0], groups[1])
	}
	readErr = errors.New("docker unavailable")
	names.next = time.Time{}
	groups = []*Container{{ID: full}}
	names.apply(context.Background(), groups)
	if groups[0].Name != "renamed-worker" {
		t.Fatal("temporary failure erased known name")
	}
}

func TestForestIncludesTetragonContainerName(t *testing.T) {
	b := NewBuilder()
	p := proc("init", "", "/bin/sh", "", "container-id", 1, 0)
	p.Pod = &tetragon.Pod{Container: &tetragon.Container{Id: "container-id", Name: "training-worker"}}
	b.Observe(p)
	b.Observe(proc("child", "init", "/bin/sleep", "", "", 2, time.Second))
	group := onlyContainer(t, b.Snapshot(Options{}), "container-id")
	if group.Name != "training-worker" || group.ProcessCount != 2 {
		t.Fatalf("missing container metadata: %+v", group)
	}
}
