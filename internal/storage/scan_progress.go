package storage

import (
	"path/filepath"
	"sort"
	"syscall"

	"project-alpha/internal/fsutil"
)

type scanContainer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Follow resource subtrees in the existing physical walk. Shared mounts and
// nested roots must not cause extra walks or change inode accounting order.
type containerProgress struct {
	containers map[string]scanContainer
	paths      map[string][]string
	pending    map[string]bool
	ancestors  map[string]bool
	remaining  map[string]int
}

func newContainerProgress(containers []scanContainer, resources []Resource, mounts []MountInfo) *containerProgress {
	p := &containerProgress{containers: map[string]scanContainer{}, paths: map[string][]string{}, pending: map[string]bool{}, ancestors: map[string]bool{}, remaining: map[string]int{}}
	entrances := stringSet{"/": true}
	for _, m := range mounts {
		if !virtualFilesystem(m.FS) {
			entrances[m.Path] = true
		}
	}
	for _, c := range containers {
		p.containers[c.ID] = c
	}
	for _, r := range resources {
		if r.accessOnly(entrances) {
			continue
		}
		path := fsutil.Canonical(r.Path)
		for _, id := range r.Containers {
			if _, ok := p.containers[id]; ok {
				p.paths[path] = appendUnique(p.paths[path], id)
			}
		}
	}
	for path, ids := range p.paths {
		p.pending[path] = true
		for probe := filepath.Dir(path); ; probe = filepath.Dir(probe) {
			p.ancestors[probe] = true
			if probe == filepath.Dir(probe) {
				break
			}
		}
		for _, id := range ids {
			p.remaining[id]++
		}
	}
	return p
}

func (p *containerProgress) finish(path string) bool {
	if p == nil {
		return false
	}
	changed := false
	finish := func(path string) {
		if p.pending[path] {
			delete(p.pending, path)
			for _, id := range p.paths[path] {
				p.remaining[id]--
			}
			changed = true
		}
	}
	finish(path)
	// Once a subtree has finished, its remaining roots were missing, excluded,
	// unreadable or aliased. They are processed, with accounting still partial.
	if p.ancestors[path] {
		for candidate := range p.pending {
			if within(candidate, path) {
				finish(candidate)
			}
		}
	}
	return changed
}

func (p *containerProgress) addTo(v object, path string) {
	if p == nil {
		return
	}
	active := map[string]bool{}
	if path != "" {
		for probe := path; ; probe = filepath.Dir(probe) {
			for _, id := range p.paths[probe] {
				active[id] = true
			}
			if probe == filepath.Dir(probe) {
				break
			}
		}
	}
	current := []scanContainer{}
	for id := range active {
		current = append(current, p.containers[id])
	}
	sort.Slice(current, func(i, j int) bool { return current[i].ID < current[j].ID })
	remaining := 0
	for _, count := range p.remaining {
		if count > 0 {
			remaining++
		}
	}
	v["containers_total"] = len(p.containers)
	v["containers_done"] = len(p.containers) - remaining
	v["containers_remaining"] = remaining
	v["current_containers"] = current
	if len(current) > 0 {
		v["phase"] = "container"
	}
}

type scanCapacity struct {
	Total, Used, Available uint64
	Identity               object
}

func (s *Scanner) observeDevice(dev uint64, path string) {
	if _, ok := s.devices[dev]; !ok {
		s.devices[dev] = path
	}
	if _, ok := s.capacities[dev]; ok {
		return
	}
	if s.virtualDevices[dev] {
		return
	}
	if _, fs := s.mountForPath(path); virtualFilesystem(fs) {
		s.virtualDevices[dev] = true
		return
	}
	var fs syscall.Statfs_t
	if syscall.Statfs(path, &fs) != nil {
		return
	}
	block := fs.Frsize
	if block == 0 {
		block = fs.Bsize
	}
	if block > 0 && fs.Blocks > 0 && fs.Blocks >= fs.Bfree {
		s.capacities[dev] = scanCapacity{fs.Blocks * uint64(block), (fs.Blocks - fs.Bfree) * uint64(block), fs.Bavail * uint64(block), s.filesystemIdentity(dev, path)}
	}
}

func (s *Scanner) probeCapacity(path string) {
	for probe := path; ; probe = filepath.Dir(probe) {
		var st syscall.Stat_t
		if syscall.Lstat(probe, &st) == nil && st.Mode&syscall.S_IFMT != syscall.S_IFLNK {
			s.observeDevice(uint64(st.Dev), probe)
			return
		}
		if probe == filepath.Dir(probe) {
			return
		}
	}
}

// Phase-only updates (helper startup, saving) retain the latest byte counts.
func mergeScanProgress(report func(object) error) func(object) error {
	if report == nil {
		return nil
	}
	last := object{}
	return func(update object) error {
		next := object{}
		for key, value := range last {
			next[key] = value
		}
		for key, value := range update {
			next[key] = value
		}
		last = next
		return report(next)
	}
}
