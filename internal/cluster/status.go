package cluster

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"project-alpha/internal/bastion"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type memberNodeResource struct {
	NodeID      string `json:"node_id"`
	NodeName    string `json:"node_name"`
	State       string `json:"state"`
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
	Port        int    `json:"port"`
	SSHHost     string `json:"ssh_host"`
	Error       string `json:"error"`
}

type memberResourceView struct {
	MemberID string               `json:"member_id"`
	Status   string               `json:"status"`
	Access   bastion.Access       `json:"access"`
	Nodes    []memberNodeResource `json:"nodes"`
}

type memberNodeStatus struct {
	memberNodeResource
	URL             string      `json:"url"`
	Online          bool        `json:"online"`
	ConnectionError string      `json:"connection_error,omitempty"`
	Host            string      `json:"host"`
	ContainerCount  int         `json:"container_count"`
	ObservedAt      string      `json:"observed_at,omitempty"`
	Scanning        bool        `json:"scanning"`
	Containers      []Container `json:"containers"`
}

// statusPublic binds both reads and applications to the bearer token's member.
// The HTML shell is public; a member ID alone never grants access to resources.
func (h *Control) statusPublic(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if !strings.HasPrefix(r.URL.Path, "/api/status/") {
		return 0, nil, nil
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/status/"), "/")
	read := len(parts) == 1 && r.Method == "GET"
	create := len(parts) == 2 && parts[1] == "containers" && r.Method == "POST"
	if !identifier.MatchString(parts[0]) || !read && !create {
		return 0, nil, httpapi.NewError(404, "接口不存在")
	}
	id, err := h.memberID(r)
	if err != nil {
		return 0, nil, err
	}
	if id != parts[0] {
		return 0, nil, httpapi.NewError(403, "资源令牌与页面使用者不匹配，请打开本人的状态页")
	}
	if create {
		return h.dispatchMemberResource(w, r, id, "containers", false, id)
	}
	return h.memberStatus(r.Context(), id)
}

func (h *Control) memberStatus(ctx context.Context, id string) (int, any, error) {
	resources, err := h.memberResources(id)
	if err != nil {
		return 0, nil, err
	}
	nodes, err := h.nodes("worker")
	if err != nil {
		return 0, nil, err
	}
	allocations := make(map[string]memberNodeResource, len(resources.Nodes))
	for _, n := range resources.Nodes {
		allocations[n.NodeID] = n
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out := make([]memberNodeStatus, len(nodes))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, node := range nodes {
		wg.Go(func() {
			allocation, ok := allocations[node.ID]
			if !ok {
				allocation = memberNodeResource{NodeID: node.ID, State: "unallocated"}
			}
			allocation.NodeName = node.Name
			n := memberNodeStatus{memberNodeResource: allocation, URL: node.URL, Containers: []Container{}}
			defer func() { out[i] = n }()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				n.ConnectionError = "节点查询超时"
				return
			}
			var inventory Inventory
			user := platform.User{ID: id, Username: "member:" + resources.Access.Username, Role: "viewer"}
			if err := h.call(ctx, node, "GET", "/api/worker/inventory", nil, user, &inventory); err != nil {
				n.ConnectionError = err.Error()
				return
			}
			n.Online = true
			n.Host = inventory.Host
			n.ContainerCount = len(inventory.Containers)
			n.ObservedAt = inventory.ObservedAt
			n.Scanning = inventory.Active != nil
			// Only the caller's containers leave control, including containers
			// assigned by an administrator outside the registration workflow.
			for _, c := range inventory.Containers {
				if c.Owner == resources.Access.Username {
					n.Containers = append(n.Containers, c)
				}
			}
		})
	}
	wg.Wait()
	return 200, map[string]any{"member_id": id, "username": resources.Access.Username, "nodes": out, "checked_at": platform.Now()}, nil
}
