package storage

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCleanupOverlayMountParsing(t *testing.T) {
	line := `42 1 0:80 / /docker/with\040space/merged rw,relatime shared:9 - overlay overlay rw,lowerdir=/lower/a\072b:/lower/c\054d,upperdir=/docker/with\040space/diff,workdir=/docker/with\040space/work`
	mounts, err := readCleanupMounts(strings.NewReader(line))
	if err != nil || len(mounts) != 1 {
		t.Fatal(mounts, err)
	}
	want := cleanupMount{ID: 42, Root: "/", Path: "/docker/with space/merged", FS: "overlay", Upper: "/docker/with space/diff", Work: "/docker/with space/work", Lower: []string{"/lower/a:b", "/lower/c,d"}}
	if !reflect.DeepEqual(mounts[0], want) {
		t.Fatalf("mount options misparsed: %#v", mounts[0])
	}
	long := "43 1 0:81 / /merged ro - overlay overlay rw,lowerdir=" + strings.Repeat("/lower/long:", 10000) + "/last,upperdir=/upper\n"
	mounts, err = readCleanupMounts(strings.NewReader(long))
	if err != nil || len(mounts) != 1 || !mounts[0].ReadOnly || mounts[0].Upper != "/upper" || len(mounts[0].Lower) != 10001 {
		t.Fatal("truncated long overlay mount options", err)
	}
	if _, err := readCleanupMounts(strings.NewReader("invalid mountinfo\n")); err == nil {
		t.Fatal("accepted invalid mount information")
	}
}

func TestCleanupContainerMappingFailsClosed(t *testing.T) {
	upper := "/docker/layer/diff"
	recorded := []cleanupContainer{{ID: strings.Repeat("a", 64), Upper: upper}}
	mount := cleanupMount{Root: "/", Path: "/docker/layer/merged", FS: "overlay", Upper: upper, Work: "/docker/layer/work", Lower: []string{"/docker/image/diff"}}
	for _, tc := range []struct {
		name, path, message string
		containers          []cleanupContainer
	}{
		{"mapped", upper + "/tmp", "", recorded},
		{"host", "/data/cache", "", recorded},
		{"unknown", upper + "/tmp", "未记录", nil},
		{"root", upper, "可写层根", recorded},
		{"ancestor", "/docker/layer", "可写层根", recorded},
		{"lower", "/docker/image/diff/cache", "镜像层", recorded},
		{"work", "/docker/layer/work/cache", "工作目录", recorded},
		{"ambiguous", upper + "/tmp", "不唯一", append(append([]cleanupContainer{}, recorded...), recorded...)},
		{"unmounted unknown", "/docker/unrecorded/diff/cache", "Docker 数据目录", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cleanupContainerForPath(tc.path, tc.containers, "/docker", []cleanupMount{mount})
			if tc.message != "" {
				if err == nil || !strings.Contains(err.Error(), tc.message) {
					t.Fatal(got, err)
				}
				return
			}
			if err != nil || (tc.name == "mapped" && got == nil) || (tc.name == "host" && got != nil) {
				t.Fatal(got, err)
			}
		})
	}
	got, err := cleanupContainerForPath(upper+"/tmp", recorded, "/docker", nil)
	if err != nil || got == nil {
		t.Fatal("mapping must not depend on host merged mount", got, err)
	}
	for _, root := range []string{"", "/elsewhere", "/", upper} {
		if _, err := cleanupContainerForPath(upper+"/tmp", recorded, root, nil); err == nil {
			t.Fatalf("accepted inconsistent Docker data root: %q", root)
		}
	}
	stale := []cleanupContainer{{ID: recorded[0].ID, Upper: upper + "-changed"}}
	if _, err := cleanupContainerForPath(upper+"/tmp", stale, "/docker", nil); err == nil {
		t.Fatal("stale metadata fell back to raw Docker data without a visible mount")
	}
}

func TestCleanupProtectsContainerNamespaceMounts(t *testing.T) {
	upper := filepath.Join(t.TempDir(), "diff")
	snapshot := &Snapshot{Scan: object{}, Containers: []Container{{UpperPath: &upper, Mounts: []ContainerMount{{Destination: "/root/.cache/mounted"}}}}}
	trees, roots := cleanupProtectedPaths(snapshot, filepath.Join(t.TempDir(), "data"), nil)
	for _, suffix := range []string{"root/.cache", "root/.cache/mounted", "root/.cache/mounted/file"} {
		if err := validateCleanupLocation(trees, roots, nil, filepath.Join(upper, suffix)); err == nil {
			t.Fatalf("accepted container namespace mount: %s", suffix)
		}
	}
	if err := validateCleanupLocation(trees, roots, nil, filepath.Join(upper, "root/.cache/unmounted")); err != nil {
		t.Fatal(err)
	}
}
