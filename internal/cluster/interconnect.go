package cluster

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"time"

	"project-alpha/internal/containers"
	"project-alpha/internal/httpapi"
	link "project-alpha/internal/interconnect"
	"project-alpha/internal/platform"
)

type linkDelivery struct {
	Node
	Subnet     string                     `json:"subnet"`
	Desired    int64                      `json:"desired"`
	Applied    int64                      `json:"applied"`
	Error      string                     `json:"error,omitempty"`
	Capability link.Status                `json:"capability"`
	Candidates []containers.LinkCandidate `json:"candidates"`
}
type linkConnection struct {
	link.Connection
	Revision int64 `json:"revision"`
	Removing bool  `json:"removing"`
}
type linkView struct {
	Pool        netip.Prefix     `json:"pool"`
	Revision    int64            `json:"revision"`
	Nodes       []linkDelivery   `json:"nodes"`
	Endpoints   []link.Endpoint  `json:"endpoints"`
	Connections []linkConnection `json:"connections"`
}

func (h *Control) linkView() (linkView, error) {
	v := linkView{Nodes: []linkDelivery{}, Endpoints: []link.Endpoint{}, Connections: []linkConnection{}}
	var pool string
	if err := h.DB.SQL.QueryRow("SELECT pool,revision FROM link_settings").Scan(&pool, &v.Revision); err != nil {
		return v, err
	}
	var err error
	v.Pool, err = netip.ParsePrefix(pool)
	if err != nil {
		return v, err
	}
	nodes, err := h.nodes("worker")
	if err != nil {
		return v, err
	}
	for _, n := range nodes {
		d := linkDelivery{Node: n}
		err = h.DB.SQL.QueryRow("SELECT subnet,desired,applied,error FROM link_nodes WHERE node=?", n.ID).Scan(&d.Subnet, &d.Desired, &d.Applied, &d.Error)
		if err != nil && err != sql.ErrNoRows {
			return v, err
		}
		v.Nodes = append(v.Nodes, d)
	}
	rows, err := h.DB.SQL.Query("SELECT e.node,e.container,e.ip,e.name,e.owner,e.removing,n.internal_ip FROM link_endpoints e JOIN cluster_nodes n ON n.id=e.node ORDER BY e.node,e.container")
	if err != nil {
		return v, err
	}
	for rows.Next() {
		var e link.Endpoint
		var ip, internal string
		if err = rows.Scan(&e.Node, &e.Container, &ip, &e.Name, &e.Owner, &e.Removing, &internal); err == nil {
			e.IP, err = netip.ParseAddr(ip)
		}
		if err == nil {
			e.Internal, err = netip.ParseAddr(internal)
		}
		if err != nil {
			rows.Close()
			return v, err
		}
		v.Endpoints = append(v.Endpoints, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return v, err
	}
	rows, err = h.DB.SQL.Query("SELECT config,revision,removing FROM link_connections ORDER BY id")
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var l linkConnection
		var s string
		if err = rows.Scan(&s, &l.Revision, &l.Removing); err != nil {
			return v, err
		}
		if err = json.Unmarshal([]byte(s), &l.Connection); err != nil {
			return v, err
		}
		v.Connections = append(v.Connections, l)
	}
	return v, rows.Err()
}
func (v linkView) config(n linkDelivery) link.Config {
	subnet, _ := netip.ParsePrefix(n.Subnet)
	ip, _ := netip.ParseAddr(n.InternalIP)
	c := link.Config{Protocol: link.Protocol, Node: n.ID, Revision: n.Desired, Pool: v.Pool, Subnet: subnet, Internal: ip}
	// A worker without an interconnect IPv4 can only withdraw. This lets an
	// administrator release an unusable reservation through the normal ACK path.
	if !ip.Is4() {
		return c
	}
	refs := map[link.Ref]bool{}
	for _, l := range v.Connections {
		if l.Removing || !l.Enabled || (l.A.Node != n.ID && l.B.Node != n.ID) {
			continue
		}
		c.Connections = append(c.Connections, l.Connection)
		refs[l.A] = true
		refs[l.B] = true
	}
	for _, e := range v.Endpoints {
		if !e.Removing && (e.Node == n.ID || refs[e.Ref]) {
			c.Endpoints = append(c.Endpoints, e)
		}
	}
	return c
}
func (v linkView) retired(n linkDelivery) bool {
	return n.Desired > 0 && n.Desired == n.Applied &&
		!slices.ContainsFunc(v.Endpoints, func(e link.Endpoint) bool { return e.Node == n.ID }) &&
		!slices.ContainsFunc(v.Connections, func(c linkConnection) bool { return c.A.Node == n.ID || c.B.Node == n.ID })
}
func (h *Control) linkActor() platform.User {
	return platform.User{ID: h.identity, Username: "interconnect-control", Role: "admin"}
}
func (h *Control) linkInfo(ctx context.Context, n *linkDelivery, c link.Config) {
	var info struct {
		Candidates []containers.LinkCandidate `json:"candidates"`
		Error      string                     `json:"error"`
		Applied    int64                      `json:"applied"`
	}
	if err := h.call(ctx, n.Node, "GET", link.WorkerPath, nil, h.linkActor(), &info); err != nil {
		n.Capability.Error = err.Error()
		return
	}
	n.Candidates = info.Candidates
	if info.Error != "" {
		n.Error = info.Error
	}
	n.Applied = info.Applied
	if !c.Internal.Is4() {
		n.Capability.Error = "容器互联需要节点配置有效的内网 IPv4 地址"
		return
	}
	if err := h.call(ctx, n.Node, "POST", link.WorkerPath, link.Request{Operation: "check", Config: c}, h.linkActor(), &n.Capability); err != nil {
		n.Capability.Error = err.Error()
	}
}
func (h *Control) linkStatus(ctx context.Context) (linkView, error) {
	v, err := h.linkView()
	if err != nil {
		return v, err
	}
	var wg sync.WaitGroup
	for i := range v.Nodes {
		c := v.config(v.Nodes[i])
		retired := v.retired(v.Nodes[i])
		wg.Go(func() {
			h.linkInfo(ctx, &v.Nodes[i], c)
			if retired {
				// A historical empty force replay can leave a worker error
				// after its withdrawal was already confirmed by the control.
				v.Nodes[i].Error = ""
			}
		})
	}
	wg.Wait()
	return v, nil
}

type linkChange struct {
	Action     string          `json:"action"`
	Revision   int64           `json:"revision"`
	Pool       netip.Prefix    `json:"pool"`
	Endpoint   link.Ref        `json:"endpoint"`
	Connection link.Connection `json:"connection"`
	ID         string          `json:"id"`
	Probe      link.Probe      `json:"probe"`
}

func (h *Control) linkDispatch(w http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
	if u.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "容器互联需要管理员权限")
	}
	h.linkMu.Lock()
	defer h.linkMu.Unlock()
	if r.Method == "GET" {
		v, e := h.linkStatus(r.Context())
		return 200, v, e
	}
	if r.Method != "POST" {
		return 0, nil, httpapi.NewError(405, "方法无效")
	}
	var op linkChange
	if err := httpapi.DecodeBody(w, r, &op); err != nil {
		return 0, nil, err
	}
	v, err := h.linkView()
	if err != nil {
		return 0, nil, err
	}
	if op.Action == "check" {
		v, e := h.linkStatus(r.Context())
		return 200, v, e
	}
	if op.Action == "probe" {
		for _, n := range v.Nodes {
			if n.ID == op.Probe.From.Node {
				var result link.Status
				err = h.call(r.Context(), n.Node, "POST", link.WorkerPath, link.Request{Operation: "probe", Config: v.config(n), Probe: op.Probe}, u, &result)
				if e := platform.Audit(h.DB.SQL, u.Username, "interconnect.probe", fmt.Sprintf("%s → %s TCP %d", op.Probe.From.Container, op.Probe.To.Container, op.Probe.Port)); e != nil {
					return 0, nil, e
				}
				return 200, result, err
			}
		}
		return 0, nil, httpapi.NewError(400, "源节点不存在")
	}
	if op.Action != "retry" && op.Revision != v.Revision {
		return 0, nil, httpapi.NewError(409, "配置版本已改变，请刷新后重试")
	}
	if op.Action == "retry" {
		err = platform.Audit(h.DB.SQL, u.Username, "interconnect.retry", fmt.Sprint(v.Revision))
	} else {
		err = h.linkChange(r.Context(), v, op, u)
	}
	if err != nil {
		return 0, nil, httpapi.NewError(409, err.Error())
	}
	if err = h.deliverLinks(r.Context(), nil, op.Action == "retry"); err != nil {
		return 0, nil, err
	}
	v, err = h.linkStatus(r.Context())
	return 200, v, err
}

func (h *Control) linkChange(ctx context.Context, v linkView, op linkChange, u platform.User) error {
	rev := v.Revision + 1
	affected := map[string]bool{}
	var candidate containers.LinkCandidate
	var target linkDelivery
	if op.Action == "settings" || op.Action == "attach" {
		pool := v.Pool
		if op.Action == "settings" {
			pool = op.Pool
		}
		for _, n := range v.Nodes {
			if ip, err := netip.ParseAddr(n.InternalIP); err == nil && pool.Contains(ip) {
				return fmt.Errorf("地址池与节点 %s 的内网地址重叠", n.Name)
			}
		}
	}
	if op.Action == "attach" {
		if !op.Endpoint.Valid() {
			return fmt.Errorf("请选择完整容器身份")
		}
		for _, n := range v.Nodes {
			if n.ID == op.Endpoint.Node {
				target = n
				break
			}
		}
		if target.ID == "" {
			return fmt.Errorf("worker 不存在")
		}
		ip, err := netip.ParseAddr(target.InternalIP)
		if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() {
			return fmt.Errorf("容器互联需要节点配置有效的内网 IPv4 地址")
		}
		h.linkInfo(ctx, &target, v.config(target))
		if target.Capability.Error != "" {
			return fmt.Errorf("%s", target.Capability.Error)
		}
		for _, item := range target.Candidates {
			if item.ID == op.Endpoint.Container {
				candidate = item
				break
			}
		}
		if candidate.ID == "" {
			return fmt.Errorf("所选容器未接管或未使用本机 bridge")
		}
	}
	return h.DB.Transaction(func(tx *sql.Tx) error {
		switch op.Action {
		case "settings":
			if !link.ValidPool(op.Pool) || len(v.Endpoints) != 0 {
				return fmt.Errorf("请在接入容器前设置边界对齐的 IPv4 单播地址池（至少 /24）")
			}
			// Keep node version counters until a new endpoint needs its allocation.
			if _, err := tx.Exec("DELETE FROM link_nodes"); err != nil {
				return err
			}
			if _, err := tx.Exec("UPDATE link_settings SET pool=?", op.Pool.String()); err != nil {
				return err
			}
		case "attach":
			// Node edits use a separate lock. Check the same address we probed
			// inside the allocation transaction before the first endpoint pins it.
			var currentIP string
			if err := tx.QueryRow("SELECT internal_ip FROM cluster_nodes WHERE id=?", target.ID).Scan(&currentIP); err != nil {
				return err
			}
			if currentIP != target.InternalIP {
				return fmt.Errorf("节点内网地址已改变，请刷新后重试")
			}
			var deleting bool
			if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM members WHERE username=? AND status='deleting')", candidate.Owner).Scan(&deleting); err != nil {
				return err
			}
			if deleting {
				return fmt.Errorf("使用者资源正在回收")
			}
			used := map[string]bool{}
			for _, n := range v.Nodes {
				used[n.Subnet] = true
			}
			subnet, err := netip.ParsePrefix(target.Subnet)
			if err != nil {
				subnet, err = link.Allocate(v.Pool, 24, used)
				if err != nil {
					return err
				}
				if _, err = tx.Exec("INSERT INTO link_nodes(node,subnet,desired) VALUES(?,?,?)", target.ID, subnet.String(), rev); err != nil {
					return err
				}
			}
			used = map[string]bool{}
			for _, e := range v.Endpoints {
				used[e.Subnet().String()] = true
			}
			block, err := link.Allocate(subnet, 30, used)
			if err != nil {
				return err
			}
			_, err = tx.Exec("INSERT INTO link_endpoints(node,container,ip,name,owner) VALUES(?,?,?,?,?)", target.ID, candidate.ID, link.Offset(block.Addr(), 2).String(), candidate.Name, candidate.Owner)
			if err != nil {
				return err
			}
			affected[target.ID] = true
		case "detach":
			for _, l := range v.Connections {
				if l.A == op.Endpoint || l.B == op.Endpoint {
					return fmt.Errorf("请先删除关联连接并等待两端确认")
				}
			}
			n, err := tx.Exec("UPDATE link_endpoints SET removing=1 WHERE node=? AND container=? AND removing=0", op.Endpoint.Node, op.Endpoint.Container)
			if err != nil {
				return err
			}
			if count, _ := n.RowsAffected(); count != 1 {
				return fmt.Errorf("端点不存在或已待撤销")
			}
			affected[op.Endpoint.Node] = true
		case "save", "disable", "delete":
			l := op.Connection
			existing := false
			for _, current := range v.Connections {
				if current.ID == op.ID && op.Action != "save" || current.ID == l.ID && op.Action == "save" {
					if current.Removing {
						return fmt.Errorf("连接正在撤销")
					}
					existing = true
					if op.Action != "save" {
						l = current.Connection
						l.Enabled = false
					} else if l.A != current.A || l.B != current.B {
						return fmt.Errorf("已保存连接的端点不可更换")
					}
					break
				}
			}
			if !existing {
				if op.Action != "save" || l.ID != "" {
					return fmt.Errorf("连接不存在")
				}
				l.ID = platform.RandomHex(16)
			}
			if err := l.Validate(); err != nil {
				return err
			}
			for _, ref := range []link.Ref{l.A, l.B} {
				i := slices.IndexFunc(v.Endpoints, func(e link.Endpoint) bool { return e.Ref == ref && !e.Removing })
				if i < 0 {
					return fmt.Errorf("连接需要两个有效的接入端点")
				}
				if l.Enabled && !v.Endpoints[i].Internal.Is4() {
					return fmt.Errorf("启用连接需要两个端点都使用内网 IPv4 地址")
				}
				affected[ref.Node] = true
			}
			if _, err := tx.Exec("INSERT INTO link_connections VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET config=excluded.config,revision=excluded.revision,removing=excluded.removing", l.ID, httpapi.JSONText(l), rev, op.Action == "delete"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("未知互联操作")
		}
		for node := range affected {
			if _, err := tx.Exec("UPDATE link_nodes SET desired=?,error='' WHERE node=?", rev, node); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("UPDATE link_settings SET revision=?", rev); err != nil {
			return err
		}
		return platform.Audit(tx, u.Username, "interconnect."+op.Action, fmt.Sprintf("v%d %s", rev, httpapi.JSONText(op)))
	})
}

// A tombstone and its allocation survive loss of either HTTP acknowledgement.
func (h *Control) deliverLinks(ctx context.Context, selected func(string) bool, force bool) error {
	v, err := h.linkView()
	if err != nil {
		return err
	}
	type ack struct {
		node     string
		revision int64
		err      error
	}
	results := make(chan ack, len(v.Nodes))
	var wg sync.WaitGroup
	for _, n := range v.Nodes {
		if n.Desired == 0 || selected != nil && !selected(n.ID) || !force && n.Desired == n.Applied && n.Error == "" {
			continue
		}
		c := v.config(n)
		// A fully withdrawn node may already have uninstalled its helper. Keep
		// its version counters, but do not replay its acknowledged empty policy.
		if v.retired(n) {
			results <- ack{n.ID, n.Desired, nil}
			continue
		}
		wg.Go(func() { results <- ack{n.ID, c.Revision, h.deliverLink(ctx, n, c)} })
	}
	wg.Wait()
	close(results)
	return h.DB.Transaction(func(tx *sql.Tx) error {
		for a := range results {
			delete(h.linkRetries, a.node)
			if a.err != nil {
				var api *httpapi.Error
				if errors.As(a.err, &api) && api.Status >= 500 {
					h.linkRetries[a.node] = true
					select {
					case h.linkWake <- struct{}{}:
					default:
					}
				}
				_, err = tx.Exec("UPDATE link_nodes SET error=? WHERE node=? AND desired=?", a.err.Error(), a.node, a.revision)
			} else {
				_, err = tx.Exec("UPDATE link_nodes SET applied=?,error='' WHERE node=? AND desired=?", a.revision, a.node, a.revision)
			}
			if err != nil {
				return err
			}
		}
		if _, err = tx.Exec(`DELETE FROM link_connections WHERE removing=1 AND revision <= (SELECT applied FROM link_nodes WHERE node=json_extract(config,'$.a.node')) AND revision <= (SELECT applied FROM link_nodes WHERE node=json_extract(config,'$.b.node'))`); err != nil {
			return err
		}
		_, err = tx.Exec("DELETE FROM link_endpoints WHERE removing=1 AND node IN (SELECT node FROM link_nodes WHERE applied=desired AND error='')")
		return err
	})
}

func (h *Control) deliverLink(ctx context.Context, n linkDelivery, c link.Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	var saved struct {
		Desired  int64  `json:"desired"`
		Applied  int64  `json:"applied"`
		Error    string `json:"error"`
		Applying int64  `json:"applying"`
		Digest   string `json:"config_digest"`
	}
	if err := h.call(ctx, n.Node, "GET", link.WorkerPath, nil, h.linkActor(), &saved); err != nil {
		return err
	}
	if saved.Applying == c.Revision {
		// An apply outlives a lost HTTP response. Poll its result instead of
		// queueing another slow apply; a process restart clears this marker.
		return httpapi.NewError(503, "worker 正在应用该配置版本，等待执行结果确认")
	}
	force := n.Desired == n.Applied && len(c.Local()) > 0
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(httpapi.JSONText(c))))
	if saved.Desired == c.Revision && saved.Applied == c.Revision && saved.Error == "" && saved.Digest == digest {
		if !force {
			return nil
		}
		// A force replay may fail before updating the worker's intent. Check
		// the runtime before trusting an ACK retained from an earlier success.
		var status link.Status
		err := h.call(ctx, n.Node, "POST", link.WorkerPath, link.Request{Operation: "check", Config: c}, h.linkActor(), &status)
		if err == nil && status.Protocol == link.Protocol && status.Applied == c.Revision && status.Error == "" {
			return nil
		}
		var apiErr *httpapi.Error
		if errors.As(err, &apiErr) && apiErr.Status >= 500 {
			return err
		}
	}
	var status link.Status
	if err := h.call(ctx, n.Node, "POST", link.WorkerPath, link.Request{Operation: "apply", Config: c, Force: force}, h.linkActor(), &status); err != nil {
		return err
	}
	if status.Applied != c.Revision || status.Protocol != link.Protocol || status.Error != "" {
		return fmt.Errorf("worker 未确认配置版本")
	}
	return nil
}

func (h *Control) recoverLinks(ctx context.Context, only string) error {
	h.linkMu.Lock()
	defer h.linkMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	return h.deliverLinks(ctx, func(id string) bool { return id == only }, true)
}

func checkLinkNode(tx *sql.Tx, id string) error {
	var linked bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM link_endpoints WHERE node=?)", id).Scan(&linked); err != nil {
		return err
	}
	if linked {
		return httpapi.NewError(409, "请先退出节点上的互联端点")
	}
	return nil
}

func (h *Control) checkLinkEndpoint(node, id string) error {
	var linked bool
	if err := h.DB.SQL.QueryRow("SELECT EXISTS(SELECT 1 FROM link_endpoints WHERE node=? AND container=?)", node, id).Scan(&linked); err != nil {
		return err
	}
	if linked {
		return httpapi.NewError(409, "请先完成容器互联清理")
	}
	return nil
}

// Replay durable pending revisions at startup. After a communication failure,
// retry only the affected deliveries, even when no administrator is connected.
func (h *Control) linkRecovery(ctx context.Context) {
	defer close(h.linkDone)
	scanAll := true
	delay := time.Second
	for ctx.Err() == nil {
		h.linkMu.Lock()
		batch := h.linkRetries
		h.linkRetries = make(map[string]bool)
		var err error
		if scanAll {
			err = h.deliverLinks(ctx, nil, false)
		} else if len(batch) > 0 {
			err = h.deliverLinks(ctx, func(id string) bool { return batch[id] }, false)
		}
		scanAll = err != nil
		pending := scanAll || len(h.linkRetries) > 0
		// While holding linkMu, every wakeup so far is covered by pending.
		select {
		case <-h.linkWake:
		default:
		}
		h.linkMu.Unlock()
		if err != nil && ctx.Err() == nil {
			log.Printf("互联恢复: %v", err)
		}
		if !pending {
			delay = time.Second
			select {
			case <-ctx.Done():
				return
			case <-h.linkWake:
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(2*delay, 30*time.Second)
	}
}
