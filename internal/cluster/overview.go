package cluster

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"project-alpha/internal/members"
	"project-alpha/internal/platform"
	"project-alpha/internal/registry"
)

type NodeStatus struct {
	Node
	Online     bool                 `json:"online"`
	Error      string               `json:"error,omitempty"`
	Inventory  *Inventory           `json:"inventory,omitempty"`
	Connection *registry.LinkStatus `json:"connection,omitempty"`
}
type MemberNode struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Containers []Container `json:"containers"`
}
type MemberSummary struct {
	Username   string       `json:"username"`
	Registered bool         `json:"registered"`
	Count      int          `json:"count"`
	Nodes      []MemberNode `json:"nodes"`
}

func (h *Control) overview(r *http.Request, user platform.User) (int, any, error) {
	nodes, err := h.nodes("")
	if err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	out := make([]NodeStatus, len(nodes))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, n := range nodes {
		wg.Go(func() {
			status := NodeStatus{Node: n}
			defer func() { out[i] = status }()
			if n.Kind == "registry" {
				connection := h.registryStatus(n.ID)
				status.Connection = &connection
				status.Online = connection.State == "connected"
				status.Error = connection.Error
				return
			}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				status.Error = "节点查询超时"
				return
			}
			var inventory Inventory
			if err := h.call(ctx, n, "GET", "/api/worker/inventory", nil, user, &inventory); err != nil {
				status.Error = err.Error()
			} else {
				status.Online = true
				status.Inventory = &inventory
			}
		})
	}
	wg.Wait()
	summary := map[string]*MemberSummary{}
	// Only admins can see the central member directory, including unused users.
	if user.Role == "admin" {
		list, err := (&members.Store{Database: h.DB}).Members()
		if err != nil {
			return 0, nil, err
		}
		for _, m := range list {
			summary[m.Username] = &MemberSummary{Username: m.Username, Registered: true, Nodes: []MemberNode{}}
		}
	}
	online, total := 0, 0
	workerPartial := false
	for _, status := range out {
		if !status.Online {
			if status.Kind == "worker" {
				workerPartial = true
			}
			continue
		}
		online++
		if status.Kind != "worker" {
			continue
		}
		perOwner := map[string][]Container{}
		for _, c := range status.Inventory.Containers {
			perOwner[c.Owner] = append(perOwner[c.Owner], c)
			total++
		}
		for owner, containers := range perOwner {
			s := summary[owner]
			if s == nil {
				s = &MemberSummary{Username: owner, Nodes: []MemberNode{}}
				summary[owner] = s
			}
			s.Count += len(containers)
			s.Nodes = append(s.Nodes, MemberNode{ID: status.ID, Name: status.Name, Containers: containers})
		}
	}
	owners := []*MemberSummary{}
	for _, s := range summary {
		owners = append(owners, s)
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i].Username < owners[j].Username })
	return 200, map[string]any{"nodes": out, "members": owners, "online": online, "container_count": total, "partial": workerPartial, "checked_at": platform.Now()}, nil
}
