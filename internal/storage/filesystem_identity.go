package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Resolve the backing disks without assuming that every filesystem is a single
// partition (device-mapper and RAID may have multiple backing devices).
func blockIdentity(sysRoot string, dev uint64) (string, []object) {
	disks := map[string]object{}
	seen := map[string]bool{}
	var visit func(string)
	visit = func(path string) {
		path, err := filepath.EvalSymlinks(path)
		if err != nil || seen[path] {
			return
		}
		seen[path] = true
		if _, err := os.Stat(filepath.Join(path, "partition")); err == nil {
			visit(filepath.Dir(path))
			return
		}
		slaves, err := os.ReadDir(filepath.Join(path, "slaves"))
		if err != nil {
			return // Unavailable topology must not invent a physical disk.
		}
		if len(slaves) > 0 {
			for _, slave := range slaves {
				visit(filepath.Join(path, "slaves", slave.Name()))
			}
			return
		}
		if strings.Contains(path, "/virtual/") {
			return
		}
		name := "/dev/" + filepath.Base(path)
		model, _ := os.ReadFile(filepath.Join(path, "device", "model"))
		disks[name] = object{"device": name, "model": strings.TrimSpace(string(model))}
	}
	path, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "dev", "block", fmt.Sprintf("%d:%d", unix.Major(dev), unix.Minor(dev))))
	if err != nil {
		return "", []object{}
	}
	visit(path)
	names := make([]string, 0, len(disks))
	for name := range disks {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]object, 0, len(names))
	for _, name := range names {
		result = append(result, disks[name])
	}
	return "/dev/" + filepath.Base(path), result
}

func (s *Scanner) filesystemIdentity(dev uint64, path string) object {
	mount, fs := s.mountForPath(path)
	device, disks := blockIdentity("/sys", dev)
	return object{"device": strconv.FormatUint(dev, 10), "mount": mount, "fs": fs, "block_device": device, "physical_disks": disks}
}

func (s *Scanner) capacityFilesystems() []object {
	devices := make([]uint64, 0, len(s.capacities))
	for dev := range s.capacities {
		devices = append(devices, dev)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i] < devices[j] })
	rows := make([]object, 0, len(devices))
	for _, dev := range devices {
		c := s.capacities[dev]
		row := object{}
		for key, value := range c.Identity {
			row[key] = value
		}
		row["total"], row["used"], row["available"] = c.Total, c.Used, c.Available
		rows = append(rows, row)
	}
	return rows
}
