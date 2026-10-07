// Package procfs reads Linux process timestamps shared by node monitors.
package procfs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ClockTicks is Linux USER_HZ, the unit used by /proc/<pid>/stat.
const ClockTicks = 100

// BootTime reads the host boot timestamp, or returns zero if it is unavailable.
func BootTime(root string) time.Time {
	data, err := os.ReadFile(filepath.Join(root, "stat"))
	if err != nil {
		return time.Time{}
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		value, found := strings.CutPrefix(line, "btime ")
		if !found {
			continue
		}
		seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || seconds <= 0 {
			return time.Time{}
		}
		return time.Unix(seconds, 0)
	}
	return time.Time{}
}

// ParseStat pulls the state and start time out of a /proc/<pid>/stat line. The
// comm field is parenthesised and may itself contain spaces and parentheses, so
// the remaining fields are counted from the last ')' rather than split naively.
func ParseStat(data []byte) (state byte, ticks uint64, ok bool) {
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
