package cluster

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"project-alpha/internal/bastion"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

type memberNodeResource struct {
	Mode        string `json:"mode"`
	TargetID    string `json:"target_id"`
	NodeID      string `json:"node_id"`
	NodeName    string `json:"node_name"`
	State       string `json:"state"`
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
	Port        int    `json:"port"`
	InternalIP  string `json:"internal_ip"`
	Error       string `json:"error"`
}

type memberResourceView struct {
	MemberID string               `json:"member_id"`
	Status   string               `json:"status"`
	Access   bastion.Access       `json:"access"`
	Control  memberControlAccess  `json:"control"`
	Nodes    []memberNodeResource `json:"nodes"`
}

type memberControlAccess struct {
	StatusURL string `json:"status_url"`
}

type memberNodeStatus struct {
	memberNodeResource
	Online          bool                 `json:"online"`
	ConnectionError string               `json:"connection_error,omitempty"`
	Host            string               `json:"host"`
	ContainerCount  int                  `json:"container_count"`
	ObservedAt      string               `json:"observed_at,omitempty"`
	Scanning        bool                 `json:"scanning"`
	Containers      []Container          `json:"containers"`
	Candidates      []containerCandidate `json:"candidates"`
	CandidatesError string               `json:"candidates_error,omitempty"`
}

var statusAPIRoute = regexp.MustCompile(`^/api/status/([a-z][a-z0-9_-]{2,31})(/containers|/login|/logout|/password)?$`)

// statusPublic binds the username in both reads and applications to the login session's member.
// The HTML shell is public; a username alone never grants access to resources.
func (h *Control) statusPublic(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if !strings.HasPrefix(r.URL.Path, "/api/status/") {
		return 0, nil, nil
	}
	parts := statusAPIRoute.FindStringSubmatch(r.URL.Path)
	if parts == nil {
		return 0, nil, httpapi.NewError(404, "接口不存在")
	}
	method := "POST"
	if parts[2] == "" {
		method = "GET"
	}
	if r.Method != method {
		return 0, nil, httpapi.NewError(404, "接口不存在")
	}
	if parts[2] == "/login" {
		token, err := h.Members.Login(w, r, parts[1])
		return 200, map[string]string{"session_token": token}, err
	}
	store := &members.Store{Database: h.DB}
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !bearer {
		return 0, nil, httpapi.NewError(401, "请使用注册时设置的密码登录")
	}
	id, err := store.SessionID(token)
	if err != nil {
		return 0, nil, err
	}
	var username string
	if err := h.DB.SQL.QueryRow("SELECT username FROM members WHERE id=?", id).Scan(&username); err != nil {
		return 0, nil, err
	}
	if username != parts[1] {
		return 0, nil, httpapi.NewError(403, "登录会话与页面使用者不匹配，请打开本人的状态页")
	}
	if parts[2] == "/logout" {
		return 200, map[string]bool{"ok": true}, store.Logout(token)
	}
	if parts[2] == "/password" {
		var req struct {
			CurrentPassword string `json:"current_password"`
			Password        string `json:"password"`
		}
		if err := httpapi.DecodeBody(w, r, &req); err != nil {
			return 0, nil, err
		}
		gate := h.memberGate(id)
		if !gate.TryLock() {
			return 0, nil, httpapi.NewError(409, "资源分配正在执行，请稍后修改密码")
		}
		defer gate.Unlock()
		return 200, map[string]bool{"ok": true}, store.ChangePassword(id, req.CurrentPassword, req.Password)
	}
	if parts[2] == "/containers" {
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
			n := memberNodeStatus{memberNodeResource: allocation, Containers: []Container{}}
			defer func() { out[i] = n }()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				n.ConnectionError = "节点查询超时"
				return
			}
			info, e := h.probe(ctx, node)
			if e != nil {
				n.ConnectionError = e.Error()
				return
			}
			if info.ID != node.ID {
				n.ConnectionError = "节点身份不匹配"
				return
			}
			n.Online = true
			if info.Protocol != Protocol {
				n.ConnectionError = "节点在线，业务协议不一致；请联系管理员更新"
				return
			}
			var inventory Inventory
			user := platform.User{ID: id, Username: "member:" + resources.Access.Username, Role: "viewer"}
			if err := h.call(ctx, node, "GET", "/api/worker/inventory", nil, user, &inventory); err != nil {
				n.ConnectionError = err.Error()
				return
			}
			n.Online = true
			n.Candidates = []containerCandidate{}
			if n.Mode == "" {
				candidates, err := h.containerCandidates(ctx, node, resources.Access.Username)
				if err != nil {
					n.CandidatesError = err.Error()
				} else {
					n.Candidates = candidates
				}
			}
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
	return 200, map[string]any{"member_id": id, "username": resources.Access.Username, "control": resources.Control, "access": resources.Access, "nodes": out, "checked_at": platform.Now()}, nil
}

func memberStatusURL(a bastion.Access) string {
	if a.ShareHost == "" || a.StatusPort == 0 {
		return ""
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(a.ShareHost, strconv.Itoa(a.StatusPort)), Path: "/status/" + a.Username}).String()
}
