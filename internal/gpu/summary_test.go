package gpu

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"project-alpha/internal/platform"
)

func TestSummaryUsesProcessPresenceAndPreservesUnknownState(t *testing.T) {
	m := &Monitor{current: Snapshot{At: platform.Now(), Devices: []Device{
		{UUID: "idle", Index: 0, Name: "Idle GPU", Processes: []Process{}},
		{UUID: "busy", Index: 1, Name: "Busy GPU", Utilization: ptr(0), Processes: []Process{{Owner: "alice"}, {Owner: "alice"}, {Owner: "host:root"}, {Owner: ""}}},
	}}}
	s := m.Summary()
	if s.Stale || len(s.Devices) != 2 || s.Devices[0].State != "idle" || s.Devices[1].State != "busy" || s.Devices[0].ProcessCount != 0 || s.Devices[1].ProcessCount != 4 {
		t.Fatalf("wrong occupancy: %+v", s)
	}
	owners := s.Devices[1].Owners
	if len(owners) != 3 || owners[0] != "alice" || owners[1] != "host:root" || owners[2] != "未识别" {
		t.Fatalf("wrong owners: %v", owners)
	}
	s.Devices[1].Owners[0] = "changed"
	if m.current.Devices[1].Processes[0].Owner != "alice" {
		t.Fatal("summary mutated monitor sample")
	}
	for _, tc := range []struct {
		name  string
		at    float64
		error string
	}{
		{"not sampled", 0, ""},
		{"stale", platform.Now() - 46, ""},
		{"failed", platform.Now(), "采集失败"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m.current.At, m.current.Error = tc.at, tc.error
			s := m.Summary()
			if !s.Stale || s.Error != tc.error || len(s.Devices) != 2 {
				t.Fatalf("lost unavailable state or last devices: %+v", s)
			}
			for _, d := range s.Devices {
				if d.State != "unknown" {
					t.Fatalf("unavailable GPU reported as %s", d.State)
				}
			}
		})
	}
	m.current = Snapshot{At: platform.Now()}
	if s := m.Summary(); s.Stale || s.Devices == nil || len(s.Devices) != 0 {
		t.Fatalf("no GPUs is not a collection failure: %+v", s)
	}
}

func TestOverviewAndSummaryShareGPUStatus(t *testing.T) {
	m := testMonitor(t)
	for _, tc := range []struct {
		name, error, state string
		age                float64
	}{
		{"fresh", "", "busy", 0},
		{"expired", "", "unknown", 46},
		{"failed", "采集失败", "unknown", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m.current = sample(platform.Now()-tc.age, "alice", "alice", "host:root")
			m.current.Error = tc.error
			code, value, err := m.Dispatch(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/gpu/overview", nil), platform.User{})
			if err != nil || code != 200 {
				t.Fatalf("overview: %d %v", code, err)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				Current struct {
					Stale   bool `json:"stale"`
					Devices []struct {
						UUID         string    `json:"uuid"`
						State        string    `json:"state"`
						ProcessCount int       `json:"process_count"`
						Owners       []string  `json:"owners"`
						Processes    []Process `json:"processes"`
					} `json:"devices"`
				} `json:"current"`
			}
			if err := json.Unmarshal(raw, &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Current.Devices) != 1 {
				t.Fatalf("lost device details: %s", raw)
			}
			d := response.Current.Devices[0]
			summary := m.Summary()
			if d.UUID != "GPU-a" || d.State != tc.state || d.State != summary.Devices[0].State || response.Current.Stale != summary.Stale || d.ProcessCount != 3 || len(d.Processes) != 3 || len(d.Owners) != 2 || d.Owners[0] != "alice" || d.Owners[1] != "host:root" {
				t.Fatalf("management and summary disagree or lost details: %s", raw)
			}
		})
	}
}
