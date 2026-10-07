package cluster

import (
	"context"
	"fmt"
	"sync"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type containerCandidate struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	MemberID  string `json:"member_id,omitempty"`
	ClaimedBy string `json:"claimed_by,omitempty"`
	Claimed   bool   `json:"claimed"`
}
type nodeChoices struct {
	NodeID     string               `json:"node_id"`
	NodeName   string               `json:"node_name"`
	Containers []containerCandidate `json:"containers"`
	Error      string               `json:"error,omitempty"`
}

func (h *Control) containerCandidates(ctx context.Context, node Node, username string) ([]containerCandidate, error) {
	var result struct {
		Containers []containerCandidate `json:"containers"`
		Error      string               `json:"error"`
	}
	if err := h.call(ctx, node, "GET", "/api/containers/candidates", nil, platform.User{ID: h.identity, Username: "provisioner", Role: "admin"}, &result); err != nil {
		return nil, err
	}
	owners, err := platform.Rows(h.DB.SQL, "SELECT username FROM members WHERE username<>?", username)
	if err != nil {
		return nil, err
	}
	registered := map[string]bool{}
	for _, row := range owners {
		registered[row["username"].(string)] = true
	}
	for i := range result.Containers {
		c := &result.Containers[i]
		c.Claimed = c.MemberID != "" || registered[c.Owner]
		if c.Claimed && c.ClaimedBy == "" {
			c.ClaimedBy = c.Owner
		}
	}
	if result.Containers == nil {
		result.Containers = []containerCandidate{}
	}
	if result.Error != "" {
		return result.Containers, httpapi.NewError(409, result.Error)
	}
	return result.Containers, nil
}
func (h *Control) registrationOptions(ctx context.Context, username string) (any, error) {
	nodes, err := h.nodes("worker")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out := make([]nodeChoices, len(nodes))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, node := range nodes {
		wg.Go(func() {
			item := nodeChoices{NodeID: node.ID, NodeName: node.Name, Containers: []containerCandidate{}}
			defer func() { out[i] = item }()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				item.Error = "节点查询超时"
				return
			}
			candidates, e := h.containerCandidates(ctx, node, username)
			if candidates != nil {
				item.Containers = candidates
			}
			if e != nil {
				item.Error = e.Error()
			}
		})
	}
	wg.Wait()
	return map[string]any{"nodes": out}, nil
}
func (h *Control) checkAdoption(ctx context.Context, node Node, username, memberID, target string) (string, error) {
	candidates, err := h.containerCandidates(ctx, node, username)
	if err != nil {
		return "", err
	}
	for _, c := range candidates {
		if c.ID != target {
			continue
		}
		if c.Claimed && c.MemberID != memberID {
			return "", httpapi.NewError(409, "容器已领养，不能重复领养")
		}
		return c.Owner, nil
	}
	return "", httpapi.NewError(409, fmt.Sprintf("容器 %s 已不存在，请刷新节点容器列表", target))
}
