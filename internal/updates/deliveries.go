package updates

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"

	"project-alpha/internal/updater"
)

type nodeDelivery struct {
	Release   Release `json:"release"`
	Delivered bool    `json:"delivered"`
}
type deliveryJournal struct {
	Format int                                `json:"format"`
	Nodes  map[string]map[string]nodeDelivery `json:"nodes"`
}

func (m *Manager) loadDeliveries() error {
	m.deliveries = deliveryJournal{Format: 1, Nodes: map[string]map[string]nodeDelivery{}}
	raw, err := os.ReadFile(filepath.Join(m.directory, "update-deliveries.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved deliveryJournal
	if json.Unmarshal(raw, &saved) != nil || saved.Format != 1 || saved.Nodes == nil {
		return fmt.Errorf("invalid update delivery journal; use a new data directory")
	}
	for id, channels := range saved.Nodes {
		if id == "" || len(channels) == 0 {
			return fmt.Errorf("invalid update delivery target; use a new data directory")
		}
		for channel, delivery := range channels {
			if delivery.Release.Validate() != nil || channel != releaseChannel(delivery.Release) {
				return fmt.Errorf("invalid queued update release; use a new data directory")
			}
		}
	}
	m.deliveries = saved
	return nil
}
func releaseChannel(r Release) string {
	if r.Prerelease {
		return "prerelease"
	}
	return "stable"
}
func (m *Manager) commitDeliveries(s deliveryJournal) error {
	if err := updater.WriteJSON(filepath.Join(m.directory, "update-deliveries.json"), s); err != nil {
		return err
	}
	m.deliveries = s
	return nil
}

// QueueDeliveries persists each worker's obligation before control acknowledges
// registry. Retain the newest publication per channel, including receipts, so
// duplicate/reordered messages do not requeue acknowledged work. Keeping stable
// and prerelease separately lets workers choose their own channel.
func (m *Manager) QueueDeliveries(r Release, ids []string) error {
	if err := r.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Repo != m.state.Config.Repo {
		return fmt.Errorf("release repository differs from update settings")
	}
	s := deliveryJournal{Format: 1, Nodes: make(map[string]map[string]nodeDelivery, len(ids))}
	for _, id := range ids {
		channels := maps.Clone(m.deliveries.Nodes[id])
		if channels == nil {
			channels = map[string]nodeDelivery{}
		}
		for channel, d := range channels {
			if d.Release.Repo != r.Repo {
				delete(channels, channel)
			}
		}
		channel := releaseChannel(r)
		old, ok := channels[channel]
		if !ok || r.Published.After(old.Release.Published) {
			channels[channel] = nodeDelivery{Release: r}
		}
		s.Nodes[id] = channels
	}
	return m.commitDeliveries(s)
}
func (m *Manager) PendingForNode(id string) []Release {
	m.mu.Lock()
	defer m.mu.Unlock()
	var releases []Release
	for _, d := range m.deliveries.Nodes[id] {
		if !d.Delivered && d.Release.Repo == m.state.Config.Repo {
			releases = append(releases, d.Release)
		}
	}
	sort.Slice(releases, func(i, j int) bool { return releases[i].Published.Before(releases[j].Published) })
	return releases
}
func (m *Manager) DeliveredToNode(id string, r Release) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	channel := releaseChannel(r)
	d, ok := m.deliveries.Nodes[id][channel]
	if !ok || d.Release.Delivery != r.Delivery || d.Delivered {
		return nil
	}
	s := m.deliveries
	s.Nodes = maps.Clone(s.Nodes)
	s.Nodes[id] = maps.Clone(s.Nodes[id])
	d.Delivered = true
	s.Nodes[id][channel] = d
	return m.commitDeliveries(s)
}
