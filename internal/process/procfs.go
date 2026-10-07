package process

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"project-alpha/internal/procfs"
)

// Allow five seconds of skew between procfs and collector start timestamps.
const startTolerance = 5 * time.Second

// Prober checks PID and start time. A false result includes unreadable procfs;
// after repeated cache misses, that process may be removed from the display.
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
	state, ticks, ok := procfs.ParseStat(data)
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
	started := boot.Add(time.Duration(int64(ticks/procfs.ClockTicks)) * time.Second)
	if delta := started.Sub(start); delta < -startTolerance || delta > startTolerance {
		// The PID was recycled by a different process; ours is no longer here.
		return false
	}
	return true
}

// bootTime reads btime from <root>/stat. It cannot change while the host is up,
// so it is read once and an unreadable procfs simply stays zero.
func (p *ProcProber) bootTime() time.Time {
	p.once.Do(func() { p.btime = procfs.BootTime(p.root) })
	return p.btime
}
