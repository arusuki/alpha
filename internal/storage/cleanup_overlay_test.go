package storage

import (
	"context"
	"os"
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

func TestCleanupOverlayMappingFailsClosed(t *testing.T) {
	upper := "/docker/layer/diff"
	base := cleanupMount{ID: 12, Root: "/", Path: "/docker/layer/merged", FS: "overlay", Upper: upper, Work: "/docker/layer/work", Lower: []string{"/docker/image/diff"}}
	for _, tc := range []struct {
		name, path, message string
		layers              []string
		mounts              []cleanupMount
	}{
		{"mapped", upper + "/root/.cache", "", []string{upper}, []cleanupMount{base}},
		{"host", "/data/cache", "", []string{upper}, []cleanupMount{base}},
		{"stopped", upper + "/root/.cache", "没有可用", []string{upper}, nil},
		{"unrecorded", upper + "/root/.cache", "未记录", nil, []cleanupMount{base}},
		{"root", upper, "可写层根", []string{upper}, []cleanupMount{base}},
		{"ancestor", "/docker/layer", "可写层根", []string{upper}, []cleanupMount{base}},
		{"ambiguous", upper + "/root/.cache", "多个合并挂载", []string{upper}, []cleanupMount{base, base}},
		{"lower", "/docker/image/diff/root/cache", "镜像层", []string{upper}, []cleanupMount{base}},
		{"work", "/docker/layer/work/work", "工作目录", []string{upper}, []cleanupMount{base}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mount, err := cleanupOverlayForPath(tc.path, tc.layers, tc.mounts)
			if tc.message != "" {
				if err == nil || !strings.Contains(err.Error(), tc.message) {
					t.Fatalf("expected %q, got %v", tc.message, err)
				}
			} else if err != nil || (tc.name == "mapped" && mount == nil) || (tc.name == "host" && mount != nil) {
				t.Fatal(mount, err)
			}
		})
	}
	base.ReadOnly = true
	if _, err := cleanupOverlayForPath(upper+"/cache", []string{upper}, []cleanupMount{base}); err == nil || !strings.Contains(err.Error(), "只读") {
		t.Fatal("accepted read-only overlay", err)
	}
	base.ReadOnly, base.Root = false, "/subdirectory"
	if _, err := cleanupOverlayForPath(upper+"/cache", []string{upper}, []cleanupMount{base}); err == nil {
		t.Fatal("mapped a partial overlay bind as a complete root")
	}
}

func TestCleanupOverlayRejectsStaleMountAndNestedMountBeforeDeletion(t *testing.T) {
	root := t.TempDir()
	upper, merged := filepath.Join(root, "diff"), filepath.Join(root, "merged")
	if err := os.Mkdir(merged, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(merged, "keep")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	mount := cleanupMount{ID: 1, Root: "/", Path: merged, Upper: upper, FS: "overlay"}
	request := cleanupHelperRequest{Paths: []string{filepath.Join(upper, "root/.cache")}, WritableLayers: []string{upper}}
	if _, err := prepareCleanupTargets(request, []cleanupMount{mount}); err == nil || !strings.Contains(err.Error(), "挂载已变化") {
		t.Fatal("accepted an ordinary directory as a live overlay", err)
	}
	nested := cleanupMount{ID: 2, Root: "/", Path: filepath.Join(merged, "root/.cache/mounted"), FS: "ext4"}
	if _, err := prepareCleanupTargets(request, []cleanupMount{mount, nested}); err == nil || !strings.Contains(err.Error(), "受保护") {
		t.Fatal("accepted a directory containing a mapped mountpoint", err)
	}
	request.ProtectedTrees = []string{filepath.Join(merged, "root/.cache/excluded")}
	if _, err := prepareCleanupTargets(request, []cleanupMount{mount}); err == nil || !strings.Contains(err.Error(), "受保护") {
		t.Fatal("ignored exclusion in merged view", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("preflight removed data", err)
	}
}

func TestCleanupOverlayUsesPinnedRootWithoutFollowingLinks(t *testing.T) {
	root := t.TempDir()
	upper, merged, outside := filepath.Join(root, "diff"), filepath.Join(root, "merged"), filepath.Join(root, "outside")
	for _, path := range []string{filepath.Join(upper, "cache"), filepath.Join(merged, "cache"), outside} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(upper, "cache/keep-physical"), filepath.Join(merged, "cache/data"), filepath.Join(outside, "keep")} {
		if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(merged, "parent-link"), filepath.Join(merged, "cache/child-link")} {
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
	}
	fd, err := os.Open(merged)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	target := cleanupTarget{Path: filepath.Join(upper, "parent-link/keep"), Relative: "parent-link/keep", Root: fd}
	if err := target.remove(context.Background()); err == nil {
		t.Fatal("followed a container parent symlink")
	}
	target.Path, target.Relative = filepath.Join(upper, "cache"), "cache"
	if err := target.remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(merged, "cache")); !os.IsNotExist(err) {
		t.Fatal("merged target remains", err)
	}
	for _, keep := range []string{filepath.Join(upper, "cache/keep-physical"), filepath.Join(outside, "keep")} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatal("deleted outside merged target", err)
		}
	}
	if err := target.remove(context.Background()); err != nil {
		t.Fatal("already absent container path failed", err)
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
