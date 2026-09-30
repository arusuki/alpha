package cluster

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"project-alpha/internal/bastion"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

func initializeProvision(tx *sql.Tx) error {
	if err := bastion.Initialize(tx); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE TABLE member_node_resources (
 member_id TEXT NOT NULL REFERENCES members(id), node_id TEXT NOT NULL REFERENCES cluster_nodes(id),
 state TEXT NOT NULL, container_id TEXT NOT NULL DEFAULT '',name TEXT NOT NULL DEFAULT '',port INTEGER NOT NULL DEFAULT 0,
 ssh_host TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', updated_at REAL NOT NULL,
 PRIMARY KEY(member_id,node_id));
 CREATE TABLE member_work (member_id TEXT PRIMARY KEY REFERENCES members(id),pending INTEGER NOT NULL);
 `)
	return err
}
func (h *Control) initProvision() error {
	h.Bastion = bastion.NewHandler(h.DB)
	h.Members.Reserve = func(tx *sql.Tx, m members.Member) error {
		if err := h.Bastion.Reserve(tx, m); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO member_node_resources(member_id,node_id,state,updated_at) SELECT ?,id,'pending',? FROM cluster_nodes WHERE kind='worker'", m.ID, platform.Now()); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO member_work VALUES(?,1)", m.ID)
		return err
	}
	h.provisionWake = make(chan struct{}, 1)
	h.Members.Registered = func(m members.Member) { h.wakeProvision() }
	if _, err := h.DB.SQL.Exec("UPDATE member_access SET invite_state='unknown',error='服务中断，邀请创建结果需核对' WHERE invite_state='creating'"); err != nil {
		return err
	}
	if _, err := h.DB.SQL.Exec("UPDATE member_work SET pending=1 WHERE member_id IN (SELECT member_id FROM member_node_resources WHERE state IN ('running','deleting'))"); err != nil {
		return err
	}
	if _, err := h.DB.SQL.Exec("UPDATE member_node_resources SET state='pending' WHERE state='running'"); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.provisionCancel = cancel
	h.provisionDone = make(chan struct{})
	go h.provisionLoop(ctx)
	return nil
}
func (h *Control) wakeProvision() {
	select {
	case h.provisionWake <- struct{}{}:
	default:
	}
}
func (h *Control) provisionLoop(ctx context.Context) {
	defer close(h.provisionDone)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		rows, err := platform.Rows(h.DB.SQL, "SELECT member_id FROM member_work WHERE pending=1 ORDER BY member_id")
		if err != nil {
			log.Printf("member queue: %v", err)
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				return
			}
			id := row["member_id"].(string)
			// Leave the work item pending through all side effects. Restart replays only
			// durable idempotent steps; ambiguous invites remain unknown.
			gate := h.memberGate(id)
			gate.Lock()
			err = h.provisionMember(ctx, id)
			if err != nil {
				log.Printf("member resource %s: %v", id, err)
			}
			if ctx.Err() == nil {
				if _, e := h.DB.SQL.Exec("UPDATE member_work SET pending=0 WHERE member_id=?", id); e != nil {
					log.Printf("member queue: %v", e)
				}
			}
			gate.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-h.provisionWake:
		case <-ticker.C:
		}
	}
}
func (h *Control) provisionMember(ctx context.Context, id string) error {
	var name, key, status string
	if err := h.DB.SQL.QueryRow("SELECT username,ssh_public_key,status FROM members WHERE id=?", id).Scan(&name, &key, &status); err != nil {
		return err
	}
	removing := status == "deleting"
	accessErr := h.Bastion.Apply(ctx, id, key, removing)
	rows, err := platform.Rows(h.DB.SQL, "SELECT node_id,state FROM member_node_resources WHERE member_id=?", id)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, row := range rows {
		state := row["state"].(string)
		if !removing && state != "pending" && state != "running" || removing && state == "deleted" {
			continue
		}
		nodeID := row["node_id"].(string)
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if e := h.applyMemberNode(ctx, id, nodeID, name, key, removing); e != nil {
				log.Printf("member resource result: %v", e)
			}
		}()
	}
	wg.Wait()
	if removing && accessErr == nil {
		return h.DB.Transaction(func(tx *sql.Tx) error {
			var count int
			if e := tx.QueryRow("SELECT count(*) FROM member_node_resources WHERE member_id=? AND state<>'deleted'", id).Scan(&count); e != nil {
				return e
			}
			if count != 0 {
				return nil
			}
			for _, table := range []string{"member_node_resources", "member_access", "member_work"} {
				if _, e := tx.Exec("DELETE FROM "+table+" WHERE member_id=?", id); e != nil {
					return e
				}
			}
			if _, e := tx.Exec("DELETE FROM members WHERE id=?", id); e != nil {
				return e
			}
			return platform.Audit(tx, "provisioner", "member.delete", name+" / "+id)
		})
	}
	return accessErr
}

// applyMemberNode runs under the member gate and persists the result before returning.
func (h *Control) applyMemberNode(ctx context.Context, id, nodeID, name, key string, removing bool) error {
	state := "running"
	method := "PUT"
	if removing {
		state = "deleting"
		method = "DELETE"
	}
	if _, e := h.DB.SQL.Exec("UPDATE member_node_resources SET state=?,error='',updated_at=? WHERE member_id=? AND node_id=?", state, platform.Now(), id, nodeID); e != nil {
		return e
	}
	node, e := h.node(nodeID)
	var out struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Port    int    `json:"port"`
		SSHHost string `json:"ssh_host"`
		OK      bool   `json:"ok"`
	}
	if e == nil {
		callCtx, cancel := context.WithTimeout(ctx, 50*time.Second)
		defer cancel()
		e = h.call(callCtx, node, method, "/api/containers/members/"+id, map[string]string{"username": name, "ssh_public_key": key}, platform.User{ID: id, Username: "member:" + name, Role: "admin"}, &out)
	}
	if e == nil && !removing && (len(out.ID) != 64 || out.Name == "" || out.Port < 1 || out.Port > 65535) {
		e = httpapi.NewError(502, "节点返回的容器分配无效，请重试核对")
	}
	if e == nil && removing && !out.OK {
		e = httpapi.NewError(502, "节点未确认回收完成")
	}
	errorText := ""
	state = "ready"
	if removing {
		state = "deleted"
	}
	if e != nil {
		state = "failed"
		errorText = e.Error()
		if ctx.Err() != nil {
			state = "pending"
			if removing {
				state = "deleting"
			}
		}
	}
	callErr := e
	if e == nil && !removing {
		_, e = h.DB.SQL.Exec("UPDATE member_node_resources SET state=?,container_id=?,name=?,port=?,ssh_host=?,error='',updated_at=? WHERE member_id=? AND node_id=?", state, out.ID, out.Name, out.Port, out.SSHHost, platform.Now(), id, nodeID)
	} else {
		_, e = h.DB.SQL.Exec("UPDATE member_node_resources SET state=?,error=?,updated_at=? WHERE member_id=? AND node_id=?", state, errorText, platform.Now(), id, nodeID)
	}
	if e != nil {
		return e
	}
	return callErr
}
func (h *Control) memberID(r *http.Request) (string, error) {
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(value) != 64 || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return "", httpapi.NewError(401, "请提供本人资源令牌")
	}
	digest := sha256.Sum256([]byte(value))
	var id string
	err := h.DB.SQL.QueryRow("SELECT id FROM members WHERE resource_token_hash=? AND status='active'", hex.EncodeToString(digest[:])).Scan(&id)
	if err == sql.ErrNoRows {
		return "", httpapi.NewError(401, "资源令牌无效或使用者正在删除")
	}
	return id, err
}
func (h *Control) memberResources(id string) (*memberResourceView, error) {
	var status string
	if err := h.DB.SQL.QueryRow("SELECT status FROM members WHERE id=?", id).Scan(&status); err != nil {
		if err == sql.ErrNoRows {
			return nil, httpapi.NewError(404, "使用者不存在")
		}
		return nil, err
	}
	a, err := h.Bastion.Access(id)
	if err != nil {
		return nil, err
	}
	rows, err := h.DB.SQL.Query(`SELECT n.id,n.name,COALESCE(a.state,'unallocated'),COALESCE(a.container_id,''),COALESCE(a.name,''),COALESCE(a.port,0),COALESCE(a.ssh_host,''),COALESCE(a.error,'') FROM cluster_nodes n LEFT JOIN member_node_resources a ON a.node_id=n.id AND a.member_id=? WHERE n.kind='worker' ORDER BY n.created_at,n.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	v := &memberResourceView{MemberID: id, Status: status, Access: a, Nodes: []memberNodeResource{}}
	for rows.Next() {
		var n memberNodeResource
		if err = rows.Scan(&n.NodeID, &n.NodeName, &n.State, &n.ContainerID, &n.Name, &n.Port, &n.SSHHost, &n.Error); err != nil {
			return nil, err
		}
		v.Nodes = append(v.Nodes, n)
	}
	return v, rows.Err()
}
func (h *Control) dispatchMemberResource(w http.ResponseWriter, r *http.Request, id, action string, admin bool, actor string) (int, any, error) {
	if action == "resources" && r.Method == "GET" {
		v, e := h.memberResources(id)
		return 200, v, e
	}
	if r.Method != "POST" && !(admin && r.Method == "DELETE" && action == "") {
		return 0, nil, httpapi.NewError(404, "接口不存在")
	}
	var req struct {
		NodeID string `json:"node_id"`
	}
	if e := httpapi.DecodeBody(w, r, &req); e != nil {
		return 0, nil, e
	}
	gate := h.memberGate(id)
	if !gate.TryLock() {
		return 0, nil, httpapi.NewError(409, "资源分配正在执行，请稍后查询或重试")
	}
	defer gate.Unlock()
	var status string
	if err := h.DB.SQL.QueryRow("SELECT status FROM members WHERE id=?", id).Scan(&status); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil, httpapi.NewError(404, "使用者不存在")
		}
		return 0, nil, err
	}
	if admin && action == "token" && status == "active" {
		token := platform.RandomHex(32)
		digest := sha256.Sum256([]byte(token))
		err := h.DB.Transaction(func(tx *sql.Tx) error {
			if _, e := tx.Exec("UPDATE members SET resource_token_hash=? WHERE id=?", hex.EncodeToString(digest[:]), id); e != nil {
				return e
			}
			return platform.Audit(tx, actor, "member.token.rotate", id)
		})
		if err != nil {
			return 0, nil, err
		}
		return 200, map[string]string{"resource_token": token}, nil
	}
	remove := admin && r.Method == "DELETE"
	if !remove && status != "active" {
		return 0, nil, httpapi.NewError(409, "使用者正在删除，不能分配新资源")
	}
	if !remove && action != "retry" && action != "containers" {
		return 0, nil, httpapi.NewError(404, "接口不存在")
	}
	if !remove && action == "containers" {
		if !identifier.MatchString(req.NodeID) {
			return 0, nil, httpapi.NewError(400, "需提供 node_id")
		}
		n, e := h.node(req.NodeID)
		if e != nil {
			return 0, nil, e
		}
		if n.Kind != "worker" {
			return 0, nil, httpapi.NewError(400, "只能在 worker 节点创建容器")
		}
		var state string
		e = h.DB.SQL.QueryRow("SELECT state FROM member_node_resources WHERE member_id=? AND node_id=?", id, req.NodeID).Scan(&state)
		if e != nil && e != sql.ErrNoRows {
			return 0, nil, e
		}
		if state == "ready" {
			v, e := h.memberResources(id)
			return 200, v, e
		}
		if _, e = h.probe(r.Context(), n); e != nil {
			return 0, nil, e
		}
	}
	err := h.DB.Transaction(func(tx *sql.Tx) error {
		if remove {
			if _, e := tx.Exec("UPDATE members SET status='deleting' WHERE id=?", id); e != nil {
				return e
			}
		} else if action == "containers" {
			_, e := tx.Exec(`INSERT INTO member_node_resources(member_id,node_id,state,updated_at) VALUES(?,?,'pending',?) ON CONFLICT(member_id,node_id) DO UPDATE SET state=CASE WHEN state='ready' THEN state ELSE 'pending' END,error='',updated_at=excluded.updated_at`, id, req.NodeID, platform.Now())
			if e != nil {
				return e
			}
		} else {
			if _, e := tx.Exec("UPDATE member_node_resources SET state='pending',error='' WHERE member_id=? AND state='failed'", id); e != nil {
				return e
			}
		}
		if _, e := tx.Exec("UPDATE member_work SET pending=1 WHERE member_id=?", id); e != nil {
			return e
		}
		event := "member.resources.retry"
		if remove {
			event = "member.delete.request"
		}
		return platform.Audit(tx, actor, event, id)
	})
	if err != nil {
		return 0, nil, err
	}
	if action == "containers" {
		// Keep the durable work marker until the queue visits it, so a process
		// interruption can recover this slot without creating another container.
		defer h.wakeProvision()
		var name, key string
		if err := h.DB.SQL.QueryRow("SELECT username,ssh_public_key FROM members WHERE id=?", id).Scan(&name, &key); err != nil {
			return 0, nil, err
		}
		if err := h.applyMemberNode(r.Context(), id, req.NodeID, name, key, false); err != nil {
			return 0, nil, err
		}
		v, e := h.memberResources(id)
		return 200, v, e
	}
	h.wakeProvision()
	v, e := h.memberResources(id)
	return 202, v, e
}
func (h *Control) memberPublic(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if !strings.HasPrefix(r.URL.Path, "/api/members/me/") {
		return 0, nil, nil
	}
	id, e := h.memberID(r)
	if e != nil {
		return 0, nil, e
	}
	return h.dispatchMemberResource(w, r, id, strings.TrimPrefix(r.URL.Path, "/api/members/me/"), false, id)
}
func memberAdminRoute(path string) (string, string, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/api/members/"), "/")
	if strings.HasPrefix(path, "/api/members/") && len(parts) <= 2 && identifier.MatchString(parts[0]) {
		a := ""
		if len(parts) == 2 {
			a = parts[1]
		}
		return parts[0], a, true
	}
	return "", "", false
}

func (h *Control) memberGate(id string) *sync.Mutex {
	v, _ := h.memberGates.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}
