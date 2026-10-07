package gpu

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"project-alpha/internal/platform"
)

const retention = 72 * time.Hour
const samplePeriod = 15 * time.Second

type Snapshot struct {
	At      float64  `json:"at"`
	Devices []Device `json:"devices"`
	Error   string   `json:"error"`
	Warning string   `json:"warning"`
}
type Monitor struct {
	db       *platform.Database
	mu       sync.RWMutex
	current  Snapshot
	previous Snapshot
	cancel   context.CancelFunc
	done     chan struct{}
	collect  func(context.Context, *platform.Database) ([]Device, string, error)
}

func NewMonitor(parent context.Context, db *platform.Database) *Monitor {
	ctx, cancel := context.WithCancel(parent)
	m := &Monitor{db: db, cancel: cancel, done: make(chan struct{}), collect: collect, current: Snapshot{Devices: []Device{}, Error: "正在检测 GPU…"}}
	go func() {
		defer close(m.done)
		m.sample(ctx)
		ticker := time.NewTicker(samplePeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.sample(ctx)
			}
		}
	}()
	return m
}
func (m *Monitor) Close() { m.cancel(); <-m.done }
func (m *Monitor) sample(ctx context.Context) {
	devices, warning, err := m.collect(ctx, m.db)
	now := platform.Now()
	next := Snapshot{At: now, Devices: devices, Warning: warning}
	if err != nil {
		next.Error = err.Error()
	}
	if err = m.persist(next); err != nil {
		next.Error = fmt.Sprintf("GPU 历史保存失败: %v", err)
		log.Print(next.Error)
	}
	m.mu.Lock()
	if next.Error != "" {
		m.current.Error = next.Error
		m.previous = Snapshot{}
	} else {
		m.current = next
		m.previous = next
	}
	m.mu.Unlock()
}

// A failed sample or service restart breaks continuity. Never fill downtime.
func (m *Monitor) persist(next Snapshot) error {
	return m.db.Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM gpu_intervals WHERE ended_at<=?", next.At-retention.Seconds()); err != nil {
			return err
		}
		previous := m.previous
		elapsed := next.At - previous.At
		if next.Error != "" || previous.At == 0 || elapsed <= 0 || elapsed > 2*samplePeriod.Seconds() {
			return nil
		}
		available := map[string]bool{}
		for _, d := range next.Devices {
			available[d.UUID] = true
		}
		for _, d := range previous.Devices {
			if !available[d.UUID] {
				continue
			}
			owners := deviceOwners(d)
			raw, err := json.Marshal(owners)
			if err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT INTO gpu_intervals(gpu_uuid,name,started_at,ended_at,utilization,owners) VALUES(?,?,?,?,?,?)", d.UUID, d.Name, previous.At, next.At, d.Utilization, string(raw)); err != nil {
				return err
			}
		}
		return nil
	})
}
func deviceOwners(d Device) []string {
	set := map[string]bool{}
	for _, p := range d.Processes {
		set[p.Owner] = true
	}
	out := []string{}
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
