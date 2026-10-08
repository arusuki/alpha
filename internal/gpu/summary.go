package gpu

import "project-alpha/internal/platform"

// Summary exposes the existing sample without collecting again or loading history.
type Summary struct {
	At      float64         `json:"at"`
	Stale   bool            `json:"stale"`
	Error   string          `json:"error"`
	Warning string          `json:"warning"`
	Devices []DeviceSummary `json:"devices"`
}

type DeviceSummary struct {
	UUID  string `json:"uuid"`
	Index int    `json:"index"`
	Name  string `json:"name"`
	DeviceStatus
}

type DeviceStatus struct {
	State        string   `json:"state"`
	ProcessCount int      `json:"process_count"`
	Owners       []string `json:"owners"`
}

func (m *Monitor) Summary() Summary {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current.summary(platform.Now())
}

func (s Snapshot) summary(now float64) Summary {
	out := Summary{At: s.At, Stale: s.At == 0 || s.Error != "" || now-s.At > 3*samplePeriod.Seconds(), Error: s.Error, Warning: s.Warning, Devices: []DeviceSummary{}}
	for _, d := range s.Devices {
		state := "idle"
		if out.Stale {
			state = "unknown"
		} else if len(d.Processes) > 0 {
			state = "busy"
		}
		out.Devices = append(out.Devices, DeviceSummary{UUID: d.UUID, Index: d.Index, Name: d.Name, DeviceStatus: DeviceStatus{State: state, ProcessCount: len(d.Processes), Owners: deviceOwners(d)}})
	}
	return out
}

type deviceView struct {
	Device
	DeviceStatus
}

type snapshotView struct {
	Summary
	Devices []deviceView `json:"devices"`
}

// The management page adds full device details to the same status as node cards.
func (s Snapshot) view(now float64) snapshotView {
	out := snapshotView{Summary: s.summary(now), Devices: []deviceView{}}
	for i, d := range s.Devices {
		out.Devices = append(out.Devices, deviceView{Device: d, DeviceStatus: out.Summary.Devices[i].DeviceStatus})
	}
	return out
}
