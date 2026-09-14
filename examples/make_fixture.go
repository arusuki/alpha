// Generate the snapshot fixture used by frontend and API tests.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"os"
	"strings"
)

type object = map[string]any

const gib = 1024 * 1024 * 1024

func leaf(path string, amount float64, files int64) object {
	parts := strings.Split(path, "/")
	return object{"name": parts[len(parts)-1], "path": path, "kind": "file", "allocated": int64(amount * gib), "apparent": int64(amount * gib), "files": files, "errors": int64(0), "children": []object{}}
}
func directory(path string, children []object) object {
	parts := strings.Split(path, "/")
	name := parts[len(parts)-1]
	if name == "" {
		name = path
	}
	n := object{"name": name, "path": path, "kind": "directory", "children": children}
	for _, key := range []string{"allocated", "apparent", "files", "errors"} {
		var sum int64
		for _, child := range children {
			sum += child[key].(int64)
		}
		n[key] = sum
	}
	return n
}
func main() {
	output := flag.String("output", "tests/fixtures/snapshot.json", "Snapshot fixture output")
	flag.Parse()
	models := directory("/srv/shared/models", []object{leaf("/srv/shared/models/Qwen3-32B", 64, 32), leaf("/srv/shared/models/DeepSeek-R1", 140, 71), leaf("/srv/shared/models/Llama-3.3-70B", 132, 62), leaf("/srv/shared/models/embeddings", 12, 120)})
	datasets := directory("/srv/shared/datasets", []object{leaf("/srv/shared/datasets/vision-corpus", 238, 84000), leaf("/srv/shared/datasets/code-instruct", 86, 3200), leaf("/srv/shared/datasets/evaluation", 24, 12800)})
	shared := directory("/srv/shared", []object{models, datasets})
	containers := []object{}
	ids := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	resources := []object{{"path": models["path"], "kinds": []string{"bind"}, "containers": ids}, {"path": datasets["path"], "kinds": []string{"bind"}, "containers": ids[:2]}}
	uppers := []object{}
	for i, item := range []struct {
		name, owner string
		amount      float64
	}{{"yuuka-train", "yuuka", 96}, {"lin-notebook", "lin", 48}, {"chen-inference", "chen", 18}} {
		cid := ids[i]
		upper := "/var/lib/docker/overlay2/" + cid[:12] + "/diff"
		uppers = append(uppers, directory(upper, []object{leaf(upper+"/workspace", item.amount*.7, 1840), leaf(upper+"/root", item.amount*.25, 890), leaf(upper+"/tmp", item.amount*.05, 43)}))
		resources = append(resources, object{"path": upper, "kinds": []string{"writable"}, "containers": []string{cid}})
		mounts := []object{{"type": "bind", "source": models["path"], "destination": "/models", "rw": false}}
		if i < 2 {
			mounts = append(mounts, object{"type": "bind", "source": datasets["path"], "destination": "/datasets", "rw": false})
		}
		state := "running"
		if i == 2 {
			state = "exited"
		}
		containers = append(containers, object{"id": cid, "name": item.name, "owner": item.owner, "image": "pytorch/pytorch:example", "state": state, "upper_path": upper, "size_rw": int64(item.amount * gib), "writable_layer": object{"allocated": uppers[i]["allocated"], "apparent": uppers[i]["apparent"], "status": "complete", "permission_denied": false}, "mounts": mounts, "log_path": nil})
	}
	logs := directory("/var/lib/docker/containers", []object{leaf("/var/lib/docker/containers/example-json.log", 8, 3)})
	resources = append(resources, object{"path": logs["path"], "kinds": []string{"container-data"}, "containers": []string{}})
	tree := directory("@root", []object{shared, directory("/var/lib/docker", append(uppers, logs))})
	tree["kind"] = "root"
	tree["name"] = "已扫描存储"
	data := object{"schema_version": 3, "revision": 0, "host": "gpu-node-01 · 示例", "finished_at": "2026-09-13T02:18:36+08:00", "docker": object{"driver": "overlay2", "version": "示例", "root": "/var/lib/docker"}, "tree": tree, "containers": containers, "resources": resources, "filesystems": []object{{"device": "demo", "mount": "/srv", "fs": "xfs", "total": int64(2048) * gib, "used": int64(1384) * gib, "available": int64(664) * gib, "reserved": 0, "scanned": tree["allocated"], "unexplained": int64(1384)*gib - tree["allocated"].(int64)}}, "warnings": []object{}, "scan": object{"max_depth": 5, "max_nodes": 50000, "error_count": 0, "omitted_references": 0, "backend": "host", "excludes": []string{}}}
	raw, err := json.Marshal(data)
	if err != nil {
		log.Fatal(err)
	}
	if err = os.WriteFile(*output, raw, 0600); err != nil {
		log.Fatal(err)
	}
}
