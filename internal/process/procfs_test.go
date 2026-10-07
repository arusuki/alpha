package process

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/procfs"
)

// fakeProcFS lays out a procfs tree so the probe can be exercised without
// reading the host's. btime is in Unix seconds, and each stat line is written
// under its own PID directory.
func fakeProcFS(t *testing.T, btime int64, processes map[uint32]string) *ProcProber {
	t.Helper()
	root := t.TempDir()
	writeProcFile(t, filepath.Join(root, "stat"), fmt.Sprintf("cpu  1 2 3 4\nbtime %d\n", btime))
	for pid, stat := range processes {
		writeProcFile(t, filepath.Join(root, strconv.FormatUint(uint64(pid), 10), "stat"), stat)
	}
	return &ProcProber{root: root}
}

func writeProcFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// statLine renders a /proc/<pid>/stat line. The state and the start time are 19
// fields apart, with 18 fields in between that the probe ignores.
func statLine(pid uint32, comm string, state byte, startTicks uint64) string {
	fields := make([]string, 20)
	fields[0] = string(state)
	for i := 1; i < 19; i++ {
		fields[i] = "0"
	}
	fields[19] = strconv.FormatUint(startTicks, 10)
	return fmt.Sprintf("%d (%s) %s\n", pid, comm, strings.Join(fields, " "))
}

func TestProcProberAlive(t *testing.T) {
	const pid = 4242
	start := epoch.Add(2 * time.Hour)
	ticks := uint64(2*time.Hour/time.Second) * procfs.ClockTicks

	cases := []struct {
		name  string
		stat  string
		start time.Time
		want  bool
	}{
		{"running and matching", statLine(pid, "daemon", 'S', ticks), start, true},
		{"start time within tolerance", statLine(pid, "daemon", 'S', ticks), start.Add(time.Second), true},
		{"start time too far off", statLine(pid, "daemon", 'S', ticks), start.Add(time.Minute), false},
		{"pid recycled later", statLine(pid, "other", 'S', ticks+uint64(time.Hour/time.Second)*procfs.ClockTicks), start, false},
		{"zombie", statLine(pid, "daemon", 'Z', ticks), start, false},
		{"already reaped", statLine(pid, "daemon", 'X', ticks), start, false},
		// The comm field is parenthesised and may itself contain spaces and
		// parentheses, so the remaining fields cannot be split naively.
		{"comm with spaces and parentheses", statLine(pid, "my prog (v2)", 'S', ticks), start, true},
		{"comm containing a close paren", statLine(pid, "weird)name", 'S', ticks), start, true},
		{"truncated stat line", fmt.Sprintf("%d (daemon) S\n", pid), start, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prober := fakeProcFS(t, epoch.Unix(), map[uint32]string{pid: c.stat})
			if got := prober.Alive(pid, c.start); got != c.want {
				t.Fatalf("Alive = %v, want %v", got, c.want)
			}
		})
	}
}

// Everything the probe cannot establish must come back false so the caller's
// miss counter keeps deciding, and the one case where existence is all we have
// must come back true so a running process is never dropped on a guess.
func TestProcProberUnknownCases(t *testing.T) {
	const pid = 4242
	start := epoch.Add(2 * time.Hour)
	ticks := uint64(2*time.Hour/time.Second) * procfs.ClockTicks
	running := statLine(pid, "daemon", 'S', ticks)

	t.Run("missing process", func(t *testing.T) {
		if fakeProcFS(t, epoch.Unix(), nil).Alive(pid, start) {
			t.Fatal("a PID with no entry must not be reported as running")
		}
	})
	t.Run("pid zero", func(t *testing.T) {
		prober := fakeProcFS(t, epoch.Unix(), map[uint32]string{pid: running})
		if prober.Alive(0, start) {
			t.Fatal("pid 0 must not be reported as running")
		}
	})
	t.Run("no reference start time", func(t *testing.T) {
		prober := fakeProcFS(t, epoch.Unix(), map[uint32]string{pid: running})
		if !prober.Alive(pid, time.Time{}) {
			t.Fatal("without a start time to compare, an existing PID must be kept")
		}
	})
	t.Run("no boot time to compare", func(t *testing.T) {
		root := t.TempDir()
		writeProcFile(t, filepath.Join(root, "stat"), "cpu  1 2 3 4\n")
		writeProcFile(t, filepath.Join(root, strconv.Itoa(pid), "stat"), running)
		if !(&ProcProber{root: root}).Alive(pid, start) {
			t.Fatal("without a boot time to compare, an existing PID must be kept")
		}
	})
	t.Run("unreadable procfs", func(t *testing.T) {
		prober := &ProcProber{root: filepath.Join(t.TempDir(), "missing")}
		if prober.Alive(pid, start) {
			t.Fatal("an unreadable procfs must not be reported as running")
		}
	})
}

// Against the host's real procfs. The expected start time is derived here with
// an explicit USER_HZ of 100, which is the point: if the probe's clock-tick or
// boot-time conversion drifted, it would place this process somewhere else and
// answer no. Every process the probe rejects would then be one it wrongly lets
// the miss counter delete.
func TestProcProberAgreesWithHostProcFS(t *testing.T) {
	prober := NewProcProber()
	boot := prober.bootTime()
	if boot.IsZero() {
		t.Skip("procfs is not readable on this host")
	}
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Skipf("cannot read /proc/self/stat: %v", err)
	}
	_, ticks, ok := procfs.ParseStat(data)
	if !ok {
		t.Fatal("could not parse /proc/self/stat")
	}
	start := boot.Add(time.Duration(int64(ticks/100)) * time.Second)

	if !prober.Alive(uint32(os.Getpid()), start) {
		t.Fatalf("the test process must be alive at its own start time %v", start)
	}
	if prober.Alive(uint32(os.Getpid()), start.Add(-time.Hour)) {
		t.Fatal("a start time an hour earlier must not match this process")
	}
	if prober.Alive(0, time.Now()) {
		t.Fatal("pid 0 must never be reported as running")
	}
}
