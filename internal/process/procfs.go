package process

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// procClockTicks is USER_HZ, the unit /proc/<pid>/stat reports starttime in.
	// The kernel fixes it at 100 for procfs regardless of the scheduler's
	// CONFIG_HZ, so it is a constant rather than something to read at runtime.
	procClockTicks = 100
	// startTolerance absorbs the gap between procfs' boot-time arithmetic and
	// the agent's timestamps. Measured against a live agent, the two disagreed by
	// 0 to 2 seconds across 1864 running processes, biased about a second, so
	// this leaves headroom rather than sitting on the boundary. Padding it only
	// guards against PID reuse, and a reused PID that lands this close to the
	// original start time merely keeps a harmless extra node; the failure in the
	// other direction would drop a running process, so err wide.
	startTolerance = 5 * time.Second
)

// Prober reports whether a PID still belongs to the process that started at the
// given time. It exists because the agent's process cache is capacity-bounded:
// it evicts live processes, and a cache miss therefore cannot mean "exited".
//
// A Prober can only keep a process in the forest. Reporting false is
// indistinguishable from "cannot tell" and falls back to the miss counter, so a
// probe that is wrong, unsupported or pointed at a missing procfs degrades to
// the previous behaviour instead of dropping a process that is still running.
type Prober interface {
	Alive(pid uint32, start time.Time) bool
}

// ProcProber answers liveness from the host's procfs. It is meaningful only
// when it reads the same PID namespace the agent reports, which holds for the
// supported deployment: the collector runs on the host and the agent shares the
// host PID namespace.
type ProcProber struct {
	root  string // procfs mount point; "/proc" in production
	once  sync.Once
	btime time.Time // boot time, fixed for the life of the boot, so read once
}

func NewProcProber() *ProcProber { return &ProcProber{root: "/proc"} }

// Alive reports whether pid is running. Anything it cannot establish counts as
// false, leaving the decision to the caller's miss counter.
func (p *ProcProber) Alive(pid uint32, start time.Time) bool {
	if pid == 0 {
		return false
	}
	data, err := os.ReadFile(filepath.Join(p.root, strconv.FormatUint(uint64(pid), 10), "stat"))
	if err != nil {
		// Gone, or not readable. Either way we cannot claim it is running.
		return false
	}
	state, ticks, ok := parseStat(data)
	if !ok || state == 'Z' || state == 'X' {
		// Zombie or already reaped: it has exited, whatever the cache says.
		return false
	}
	boot := p.bootTime()
	if start.IsZero() || boot.IsZero() {
		// Nothing to compare against. The PID exists, so keep it: an extra node
		// is harmless, whereas dropping a running process is not.
		return true
	}
	started := boot.Add(time.Duration(int64(ticks/procClockTicks)) * time.Second)
	if delta := started.Sub(start); delta < -startTolerance || delta > startTolerance {
		// The PID was recycled by a different process; ours is no longer here.
		return false
	}
	return true
}

// bootTime reads btime from <root>/stat. It cannot change while the host is up,
// so it is read once and an unreadable procfs simply stays zero.
func (p *ProcProber) bootTime() time.Time {
	p.once.Do(func() {
		data, err := os.ReadFile(filepath.Join(p.root, "stat"))
		if err != nil {
			return
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			value, found := strings.CutPrefix(line, "btime ")
			if !found {
				continue
			}
			seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return
			}
			p.btime = time.Unix(seconds, 0)
			return
		}
	})
	return p.btime
}

// parseStat pulls the state and start time out of a /proc/<pid>/stat line. The
// comm field is parenthesised and may itself contain spaces and parentheses, so
// the remaining fields are counted from the last ')' rather than split naively.
func parseStat(data []byte) (state byte, ticks uint64, ok bool) {
	line := string(data)
	end := strings.LastIndexByte(line, ')')
	if end < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(line[end+1:])
	// Fields are numbered from one and start at the state (3), so the start
	// time (22) sits nineteen entries further along.
	if len(fields) < 20 || fields[0] == "" {
		return 0, 0, false
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], ticks, true
}
