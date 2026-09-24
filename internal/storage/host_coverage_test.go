package storage

import "testing"

func TestHostCoverageReconcilesFoldedAndMixedDirectories(t *testing.T) {
	const gib = int64(1 << 30)
	file := &Node{Path: "/reported/file", Kind: "file", Allocated: 2 * gib}
	reported := &Node{Path: "/reported", Kind: "directory", Allocated: 2*gib + 4096, Children: []*Node{file}}
	safe := &Node{Path: "/home/tools/skills", Kind: "directory", Allocated: 600 << 20, Omitted: 20}
	claimed := &Node{Path: "/home/tools/dockerfile", Kind: "directory", Allocated: 4096}
	mixed := &Node{Path: "/home/tools", Kind: "directory", Allocated: safe.Allocated + claimed.Allocated, Children: []*Node{safe, claimed}}
	cuda := &Node{Path: "/usr/local/cuda", Kind: "directory", Allocated: 9 * gib}
	usr := &Node{Path: "/usr", Kind: "directory", Allocated: 20*gib + cuda.Allocated, Omitted: 100, Children: []*Node{cuda}}
	docker := &Node{Path: "/var/lib/docker", Kind: "directory", Allocated: 10 * gib, Omitted: 50}
	root := &Node{Path: "/", Kind: "directory", Allocated: 45*gib + reported.Allocated + mixed.Allocated + usr.Allocated + docker.Allocated, Omitted: 100, Children: []*Node{reported, mixed, usr, docker}}
	s := &Snapshot{Tree: &Node{Path: "@root", Kind: "root", Allocated: root.Allocated, Children: []*Node{root}}, Containers: []Container{{ID: "container"}}, Resources: []Resource{{Path: claimed.Path, Containers: []string{"container"}}, {Path: cuda.Path, Containers: []string{"container"}}}}
	view := &recordView{snapshot: s, usage: buildUsage(s)}
	result, err := view.hostCoverage([]string{"/", "/reported"}, []string{reported.Path, file.Path, reported.Path}, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if result["covered_allocated"] != reported.Allocated || result["docker_allocated"] != docker.Allocated {
		t.Fatalf("path union/Docker partition wrong: %v", result)
	}
	remaining := result["remaining"].([]object)
	byPath := map[string]object{}
	var sum int64
	for _, row := range remaining {
		byPath[row["path"].(string)] = row
		sum += row["host_allocated"].(int64)
	}
	if sum != result["remaining_allocated"] || sum+reported.Allocated+docker.Allocated != result["host_allocated"] {
		t.Fatal("coverage does not reconcile")
	}
	if byPath["/"]["host_allocated"] != 45*gib || byPath["/"]["next_tool"] != "scan_directory" || byPath["/usr"]["host_allocated"] != 20*gib || byPath[safe.Path]["next_tool"] != "get_host_directory" {
		t.Fatalf("folded root or mixed safe child lost: %v", remaining)
	}
	if byPath[docker.Path] != nil || byPath[claimed.Path] != nil {
		t.Fatal("container/Docker content became review target")
	}
	page, err := view.hostCoverage([]string{"/"}, []string{reported.Path}, 1, 1)
	if err != nil || len(page["remaining"].([]object)) != 1 || page["has_more"] != true || page["remaining_allocated"] != sum {
		t.Fatalf("pagination changes totals: %v %v", page, err)
	}
	narrow, err := view.hostCoverage([]string{mixed.Path}, nil, 0, 50)
	if err != nil || narrow["host_allocated"] != safe.Allocated {
		t.Fatalf("group scope leaked: %v %v", narrow, err)
	}
	for _, paths := range [][]string{{"/missing"}, {claimed.Path}, {docker.Path}} {
		if _, err := view.hostCoverage([]string{"/"}, paths, 0, 50); err == nil {
			t.Fatalf("accepted invalid coverage paths %v", paths)
		}
	}
}
