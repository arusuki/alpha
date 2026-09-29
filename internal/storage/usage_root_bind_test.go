package storage

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFilesystemEntranceBindDoesNotClaimMachine(t *testing.T) {
	raw, err := os.ReadFile("../../tests/fixtures/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, entrance := range []string{"/", "/boot/efi", "/data", "/mnt/arbitrary partition"} {
		for _, rw := range []bool{false, true} {
			var s Snapshot
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			s.Filesystems = append(s.Filesystems, object{"mount": entrance, "fs": "ext4"})
			// Host bytes (including folded contents) and an ordinary container
			// directory share the partition. The monitor must claim neither.
			if entrance != "/" {
				ordinary := filepath.Join(entrance, "app-data")
				s.Tree.Children = append(s.Tree.Children, &Node{Path: entrance, Kind: "directory", Allocated: 100, Children: []*Node{{Path: ordinary, Kind: "directory", Allocated: 30, Children: []*Node{}}}})
				s.Tree.Allocated += 100
				s.Resources = append(s.Resources, Resource{Path: ordinary, Kinds: []string{"bind"}, Containers: []string{s.Containers[1].ID}})
			}
			before := buildUsage(&s)
			s.Containers[0].Mounts = append(s.Containers[0].Mounts, ContainerMount{Type: "bind", Source: stringPointer(entrance), Destination: "/run/host", RW: rw})
			s.Resources = append(s.Resources, Resource{Path: entrance, Kinds: []string{"bind", "host"}, Containers: []string{s.Containers[0].ID}})
			after := buildUsage(&s)
			if after.Exclusive != before.Exclusive || after.Shared != before.Shared || after.Unrelated != before.Unrelated || !reflect.DeepEqual(after.HostAllocated, before.HostAllocated) || !reflect.DeepEqual(after.Owners, before.Owners) {
				t.Fatalf("host access changed attribution: before=%+v after=%+v", before, after)
			}
			for id, want := range before.Containers {
				got := after.Containers[id]
				if got.Exclusive != want.Exclusive || got.Shared != want.Shared || got.Partial != want.Partial || got.Known != want.Known {
					t.Fatalf("root bind changed container %s: %+v / %+v", id, got, want)
				}
			}
			// Browser and API must compute the same partition, including root binds.
			if _, err := exec.LookPath("node"); err != nil {
				t.Fatal(err)
			}
			input, _ := json.Marshal(s)
			cmd := exec.Command("node", "-e", `const u=require('./dist/usage.js').build(JSON.parse(require('fs').readFileSync(0,'utf8')));console.log(JSON.stringify({exclusive:u.exclusive,shared:u.shared,unrelated:u.unrelated}));`)
			cmd.Dir, cmd.Stdin = "../..", bytes.NewReader(input)
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			var js struct{ Exclusive, Shared, Unrelated int64 }
			if err := json.Unmarshal(out, &js); err != nil {
				t.Fatal(err)
			}
			if js.Exclusive != after.Exclusive || js.Shared != after.Shared || js.Unrelated != after.Unrelated {
				t.Fatalf("browser/API disagree: %s", out)
			}
		}
	}
}

func TestHostRootBindDoesNotClaimHostProgress(t *testing.T) {
	for _, entrance := range []string{"/", "/boot/efi", "/data", "/mnt/arbitrary partition"} {
		upper := filepath.Join(entrance, "runner")
		host := filepath.Join(entrance, "unrelated")
		p := newContainerProgress([]scanContainer{{"runner", "runner"}}, []Resource{
			{Path: entrance, Kinds: []string{"bind"}, Containers: []string{"runner"}},
			{Path: upper, Kinds: []string{"writable"}, Containers: []string{"runner"}},
		}, []MountInfo{{Path: entrance, FS: "ext4"}})
		v := object{"phase": "host"}
		p.addTo(v, host)
		if v["phase"] != "host" || len(v["current_containers"].([]scanContainer)) != 0 {
			t.Fatalf("host scan attributed to root bind: %v", v)
		}
		p.addTo(v, upper)
		if len(v["current_containers"].([]scanContainer)) != 1 {
			t.Fatalf("ordinary container resource lost progress: %v", v)
		}
		p.finish(upper)
		p.addTo(v, host)
		if v["containers_remaining"] != 0 {
			t.Fatalf("container waits for entire host: %v", v)
		}
	}
}
