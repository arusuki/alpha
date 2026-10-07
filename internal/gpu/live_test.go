package gpu

import (
	"context"
	"os"
	"testing"
	"time"

	"project-alpha/internal/platform"
)

// Explicit opt-in: observes real devices/procfs and optionally reads registered
// container ownership. It never starts a workload or writes the supplied DB.
func TestLiveNVIDIA(t *testing.T) {
	if os.Getenv("PROJECT_ALPHA_TEST_GPU") != "1" {
		t.Skip("set PROJECT_ALPHA_TEST_GPU=1 to observe local NVIDIA devices")
	}
	db := testMonitor(t).db
	if dir := os.Getenv("PROJECT_ALPHA_GPU_TEST_DATA_DIR"); dir != "" {
		existing, err := platform.OpenExistingDatabase(dir, true)
		if err != nil {
			t.Fatal(err)
		}
		defer existing.SQL.Close()
		db = existing
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	devices, warning, err := collect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) == 0 {
		t.Fatal("no NVIDIA devices available for live verification")
	}
	processes, attributed, started := 0, 0, 0
	for _, d := range devices {
		if d.UUID == "" || d.Name == "" {
			t.Fatal("missing device identity")
		}
		for _, p := range d.Processes {
			processes++
			if p.Owner != "未识别" && p.Owner != "未归属容器" {
				attributed++
			}
			if p.StartedAt != nil {
				started++
				if *p.StartedAt > platform.Now() {
					t.Fatal("future process start time")
				}
			}
		}
	}
	t.Logf("Observed %d GPUs, %d process/device pairs, %d attributed, %d with start times; warning=%q", len(devices), processes, attributed, started, warning)
}
