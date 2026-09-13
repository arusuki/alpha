package storage

import (
	"runtime"
)

// NumCPU reflects the CPUs available to the process at startup, including CPU
// affinity. Do not inherit the web service's GOMAXPROCS as a scan mode setting.
func (c Config) scanParallelism() int {
	return scanModeParallelism(c.ScanMode, runtime.NumCPU())
}

func scanModeParallelism(mode string, available int) int {
	available = max(1, available)
	if mode == "fast" {
		return available
	}
	return min(4, available)
}

// Linux counts OS threads against the container PID limit. Reserve room beyond
// runnable Go threads for GC, runtime services and threads blocked in I/O.
func helperPIDLimit(parallelism int) int {
	return max(128, 4*parallelism+32)
}
