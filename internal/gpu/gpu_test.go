package gpu

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/platform"
)

func ptr(n float64) *float64 { return &n }
func testMonitor(t *testing.T) *Monitor {
	t.Helper()
	db, err := platform.OpenDatabase(t.TempDir(), func(tx *sql.Tx) error {
		if err := platform.InstallGPUHistory(tx); err != nil {
			return err
		}
		_, err := tx.Exec("CREATE TABLE owners(container_id TEXT PRIMARY KEY,owner TEXT); CREATE TABLE managed_containers(id TEXT PRIMARY KEY,name TEXT); INSERT INTO service_identity VALUES(1,'worker','test')")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	return &Monitor{db: db}
}
func sample(at float64, owners ...string) Snapshot {
	d := Device{UUID: "GPU-a", Name: "Test GPU", Utilization: ptr(80), Processes: []Process{}}
	for _, owner := range owners {
		d.Processes = append(d.Processes, Process{Owner: owner})
	}
	return Snapshot{At: at, Devices: []Device{d}}
}
func TestHistoryUnionSharedCardsRollingAndGaps(t *testing.T) {
	m := testMonitor(t)
	now := float64(300000)
	cutoff := now - retention.Seconds()
	// A crossing interval must be clipped to the 72h window; duplicate processes count once.
	m.previous = sample(cutoff-10, "alice", "alice", "bob")
	if err := m.persist(sample(cutoff + 5)); err != nil {
		t.Fatal(err)
	}
	m.previous = sample(now-15, "alice")
	other := m.previous.Devices[0]
	other.UUID = "GPU-b"
	m.previous.Devices = append(m.previous.Devices, other)
	next := sample(now)
	other = next.Devices[0]
	other.UUID = "GPU-b"
	next.Devices = append(next.Devices, other)
	if err := m.persist(next); err != nil {
		t.Fatal(err)
	}
	// No bridge over a collection failure, delay, removal, or a service restart.
	for _, prev := range []Snapshot{{}, sample(now-60, "alice"), sample(now+1, "alice")} {
		m.previous = prev
		if err := m.persist(sample(now)); err != nil {
			t.Fatal(err)
		}
	}
	m.previous = sample(now, "alice")
	if err := m.persist(Snapshot{At: now, Error: "driver unavailable"}); err != nil {
		t.Fatal(err)
	}
	h, err := m.history(now, now-3600, now, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Users) != 2 || h.Users[0].Owner != "alice" || h.Users[0].Seconds != 35 || h.Users[1].Seconds != 5 {
		t.Fatalf("bad totals: %+v", h.Users)
	}
	if len(h.Series) != 2 || len(h.Series[0].Points) != 1 || h.Series[0].Points[0].Owners["alice"] != 15 || *h.Series[0].Points[0].Utilization != 80 {
		t.Fatalf("bad series: %+v", h)
	}
	m.previous = Snapshot{}
	if err = m.persist(Snapshot{At: now + retention.Seconds() + 1, Error: "still unavailable"}); err != nil {
		t.Fatal(err)
	}
	var count int
	m.db.SQL.QueryRow("SELECT count(*) FROM gpu_intervals").Scan(&count)
	if count != 0 {
		t.Fatalf("failed samples did not prune: %d", count)
	}
}
func TestBucketSplitsAndUnknownUtilization(t *testing.T) {
	m := testMonitor(t)
	m.previous = sample(105, "a")
	m.previous.Devices[0].Utilization = nil
	if err := m.persist(sample(120)); err != nil {
		t.Fatal(err)
	}
	h, err := m.history(120, 100, 120, 10)
	if err != nil {
		t.Fatal(err)
	}
	p := h.Series[0].Points
	if len(p) != 2 || p[0].Observed != 5 || p[1].Observed != 10 || p[0].Utilization != nil || h.Users[0].Seconds != 15 {
		t.Fatalf("bad bucket split: %+v", h)
	}
}
func TestCollectionFailureAndRecovery(t *testing.T) {
	m := testMonitor(t)
	old := sample(platform.Now()-15, "a")
	m.current = old
	m.previous = old
	m.collect = func(context.Context, *platform.Database) ([]Device, string, error) {
		return nil, "", errors.New("offline")
	}
	m.sample(context.Background())
	if m.current.At != old.At || m.current.Error != "offline" || m.previous.At != 0 {
		t.Fatalf("failure state: %+v", m.current)
	}
	m.collect = func(context.Context, *platform.Database) ([]Device, string, error) {
		return sample(0, "b").Devices, "", nil
	}
	m.sample(context.Background())
	var count int
	m.db.SQL.QueryRow("SELECT count(*) FROM gpu_intervals").Scan(&count)
	if count != 0 || m.current.Error != "" {
		t.Fatal("recovery invented history")
	}
}
func TestOverviewBoundsAndMethods(t *testing.T) {
	m := testMonitor(t)
	for _, query := range []string{"hours=0", "hours=73", "hours=NaN", "step=0", "step=Inf", "end=1"} {
		_, _, err := m.Dispatch(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/gpu/overview?"+query, nil), platform.User{})
		if err == nil {
			t.Fatalf("accepted %s", query)
		}
	}
	for _, method := range []string{"POST", "DELETE"} {
		_, _, err := m.Dispatch(httptest.NewRecorder(), httptest.NewRequest(method, "/api/gpu/overview", nil), platform.User{})
		if err == nil {
			t.Fatal("accepted mutation")
		}
	}
	status, v, err := m.Dispatch(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/gpu/overview?hours=72&step=15", nil), platform.User{})
	if err != nil || status != 200 {
		t.Fatal(status, err)
	}
	h := v.(map[string]any)["history"].(History)
	if (h.To-h.From)/h.Step > 864 {
		t.Fatal("unbounded response")
	}
}
func TestSMIParsing(t *testing.T) {
	raw := `<nvidia_smi_log><attached_gpus>2</attached_gpus><gpu><uuid>GPU-1</uuid><product_name>Test &amp; GPU</product_name><utilization><gpu_util>42 %</gpu_util></utilization><fb_memory_usage><used>123 MiB</used><total>24576 MiB</total></fb_memory_usage><temperature><gpu_temp>58 C</gpu_temp></temperature><compute_mode>Default</compute_mode><processes><process_info><pid>21</pid><type>C</type><process_name>python</process_name><used_memory>123 MiB</used_memory></process_info><process_info><pid>22</pid><type>G</type><process_name>Xorg</process_name><used_memory>N/A</used_memory></process_info></processes></gpu><gpu><uuid>GPU-2</uuid><product_name>MIG GPU</product_name><mig_mode><current_mig>Enabled</current_mig></mig_mode><utilization><gpu_util>N/A</gpu_util></utilization><processes/></gpu></nvidia_smi_log>`
	d, err := parseSMI([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 2 || *d[0].Utilization != 42 || len(d[0].Processes) != 2 || d[0].Processes[1].Memory != nil || d[1].Utilization != nil || !d[1].MIG {
		t.Fatalf("bad parse: %+v", d)
	}
	for _, raw := range []string{`<bad/>`, `<nvidia_smi_log/>`, `<nvidia_smi_log><attached_gpus>1</attached_gpus></nvidia_smi_log>`, strings.ReplaceAll(raw, "42 %", "NaN"), strings.ReplaceAll(raw, "<processes/>", "<processes>N/A</processes>")} {
		_, err := parseSMI([]byte(raw))
		if strings.Contains(raw, "NaN") {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err == nil {
			t.Fatal("accepted malformed or unsupported output")
		}
	}
}
func TestProcAttributionAndUnknown(t *testing.T) {
	root := t.TempDir()
	id := strings.Repeat("a", 64)
	for i, cgroup := range []string{"0::/system.slice/docker-" + id + ".scope\n", "0::/docker/" + id + "\n"} {
		dir := filepath.Join(root, fmt.Sprint(i+10))
		os.MkdirAll(dir, 0700)
		fields := make([]string, 20)
		for j := range fields {
			fields[j] = "0"
		}
		fields[0] = "S"
		fields[19] = "4500"
		os.WriteFile(filepath.Join(dir, "stat"), []byte(fmt.Sprintf("%d (python (train)) %s", i+10, strings.Join(fields, " "))), 0600)
		os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cgroup), 0600)
		p := Process{PID: i + 10, Owner: "未识别"}
		readProcess(root, &p, 1000)
		if p.ContainerID != id || p.Owner != "未归属容器" || p.StartedAt == nil || *p.StartedAt != 1045 {
			t.Fatalf("bad attribution: %+v", p)
		}
	}
	p := Process{PID: 123, Owner: "未识别"}
	readProcess(root, &p, 1000)
	if p.Owner != "未识别" || p.StartedAt != nil {
		t.Fatal("unreadable PID guessed")
	}
}
func TestMonitorStops(t *testing.T) {
	m := testMonitor(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	live := NewMonitor(ctx, m.db)
	done := make(chan struct{})
	go func() { live.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("monitor did not stop")
	}
}
