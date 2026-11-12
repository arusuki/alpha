package linkhost

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	link "project-alpha/internal/interconnect"
)

type route struct {
	Dst, Dev, Gateway, Prefsrc string
	Protocol                   json.RawMessage
}

func (r route) owned(o ownedRoute) bool {
	return strings.Trim(r.ProtocolString(), "\"") == "242" && strings.TrimSuffix(r.Dst, "/32") == o.Dest && r.Dev == o.Dev && r.Gateway == o.Gateway && (o.Source == "" || r.Prefsrc == o.Source)
}
func (r route) ProtocolString() string { return string(r.Protocol) }
func (h *host) routes(ns *os.File, args ...string) ([]route, error) {
	s, e := h.run(ns, "", "ip", append([]string{"-j", "-4", "route", "show"}, args...)...)
	if e != nil {
		return nil, e
	}
	var rows []route
	e = json.Unmarshal([]byte(s), &rows)
	return rows, e
}

type network struct {
	ID, Name, Driver string
	Labels, Options  map[string]string
	EnableIPv6       bool
	IPAM             struct {
		Config []struct{ Subnet, Gateway string }
	}
	Containers map[string]struct{ IPv4Address string }
}

func (h *host) networks() ([]network, error) {
	s, err := h.docker("network", "ls", "-q", "--no-trunc")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(s)
	if len(ids) == 0 {
		return nil, nil
	}
	s, err = h.docker(append([]string{"network", "inspect"}, ids...)...)
	if err != nil {
		return nil, err
	}
	var list []network
	err = json.Unmarshal([]byte(s), &list)
	return list, err
}

const ownerLabel = "org.project-alpha.interconnect.worker"
const idLabel = "org.project-alpha.interconnect.container"

func (h *host) owned(n network, e link.Endpoint) bool {
	if id := h.state.NetworkIDs[e.Container]; id != "" && n.ID != id {
		return false
	}
	if n.Driver != "bridge" || n.Name != e.Network() || n.EnableIPv6 || len(n.IPAM.Config) != 1 {
		return false
	}
	if n.Labels[ownerLabel] != h.install.Worker || n.Labels[idLabel] != e.Container {
		return false
	}
	return n.Options["com.docker.network.bridge.name"] == e.Bridge() &&
		n.Options["com.docker.network.bridge.gateway_mode_ipv4"] == "routed" &&
		n.IPAM.Config[0].Subnet == e.Subnet().String() &&
		n.IPAM.Config[0].Gateway == e.Subnet().Addr().Next().String()
}
func routePrefix(s string) (netip.Prefix, error) {
	if a, e := netip.ParseAddr(s); e == nil {
		return netip.PrefixFrom(a, 32), nil
	}
	return netip.ParsePrefix(s)
}
func (h *host) conflicts(pool netip.Prefix) error {
	nets, err := h.networks()
	if err != nil {
		return err
	}
	bridges := map[string]netip.Prefix{}
	for _, n := range nets {
		owned := false
		for _, e := range h.state.Endpoints {
			if h.owned(n, e) {
				owned = true
				bridges[e.Bridge()] = e.Subnet()
				for id := range n.Containers {
					if id != e.Container {
						return fmt.Errorf("专用网络 %s 接入了其他容器", n.Name)
					}
				}
				break
			}
		}
		if owned {
			continue
		}
		for _, p := range n.IPAM.Config {
			if subnet, err := netip.ParsePrefix(p.Subnet); err == nil && pool.Overlaps(subnet) {
				return fmt.Errorf("互联地址池与 Docker 网络 %s (%s) 重叠", n.Name, p.Subnet)
			}
		}
	}
	routes, err := h.routes(nil, "table", "all")
	if err != nil {
		return err
	}
	for _, r := range routes {
		p, e := routePrefix(r.Dst)
		if e != nil || !pool.Overlaps(p) {
			continue
		}
		if own, ok := bridges[r.Dev]; ok && own.Contains(p.Addr()) && p.Bits() >= own.Bits() {
			continue
		}
		if slices.ContainsFunc(h.state.Routes, func(o ownedRoute) bool { return o.Container == "" && r.owned(o) }) {
			continue
		}
		return fmt.Errorf("地址池与宿主机路由 %s dev %s 冲突", p, r.Dev)
	}
	return nil
}

type inspect struct {
	ID    string
	State struct {
		Pid     int
		Running bool
	}
	HostConfig      struct{ NetworkMode string }
	NetworkSettings struct {
		SandboxKey string
		Networks   map[string]struct {
			NetworkID, IPAddress, MacAddress string
			GwPriority                       int
		}
	}
}

func (h *host) inspect(id string) (inspect, error) {
	var rows []inspect
	s, err := h.docker("inspect", "--type", "container", id)
	if err == nil {
		err = json.Unmarshal([]byte(s), &rows)
	}
	if err != nil {
		return inspect{}, err
	}
	if len(rows) != 1 || rows[0].ID != id || rows[0].HostConfig.NetworkMode != "bridge" {
		return inspect{}, fmt.Errorf("容器或 bridge 身份无效")
	}
	return rows[0], nil
}

var sandboxKey = regexp.MustCompile("^/(var/)?run/docker/netns/[a-f0-9]{12,64}$")

// runningNamespace returns a nil handle for a verified stopped container.
func (h *host) runningNamespace(id string) (*os.File, inspect, error) {
	c, err := h.inspect(id)
	if err != nil {
		return nil, c, err
	}
	if !c.State.Running && c.State.Pid == 0 {
		return nil, c, nil
	}
	if !c.State.Running || c.State.Pid <= 1 || !sandboxKey.MatchString(c.NetworkSettings.SandboxKey) {
		return nil, c, fmt.Errorf("无法确认运行中容器的网络 namespace")
	}
	ns, err := os.Open(c.NetworkSettings.SandboxKey)
	if err != nil {
		return nil, c, err
	}
	pinned, e := ns.Stat()
	proc, f := os.Stat(fmt.Sprintf("/proc/%d/ns/net", c.State.Pid))
	self, g := os.Stat("/proc/self/ns/net")
	current, v := h.inspect(id)
	if e != nil || f != nil || g != nil || v != nil || current.State.Pid != c.State.Pid || current.NetworkSettings.SandboxKey != c.NetworkSettings.SandboxKey || !os.SameFile(pinned, proc) || os.SameFile(pinned, self) {
		ns.Close()
		return nil, c, fmt.Errorf("容器网络 namespace 身份改变")
	}
	return ns, c, nil
}
func (h *host) endpointNetwork(e link.Endpoint, remove bool) (network, error) {
	nets, err := h.networks()
	if err != nil {
		return network{}, err
	}
	for _, n := range nets {
		if n.Name != e.Network() {
			continue
		}
		if !h.owned(n, e) {
			return n, fmt.Errorf("网络 %s 归属或配置改变", n.Name)
		}
		for id, a := range n.Containers {
			if id != e.Container || a.IPv4Address != e.IP.String()+"/30" {
				return n, fmt.Errorf("专用网络附加状态与容器身份不一致")
			}
		}
		if remove {
			// Stopped containers retain NetworkSettings even when Docker removes
			// their live endpoints from network inspect. Disconnect them by ID
			// before deleting the network so their next start remains possible.
			ref, err := h.docker("ps", "-aq", "--no-trunc", "--filter", "id="+e.Container)
			if err != nil {
				return n, err
			}
			attached := false
			if strings.TrimSpace(ref) == e.Container {
				ins, err := h.inspect(e.Container)
				if err != nil {
					return n, err
				}
				if endpoint, ok := ins.NetworkSettings.Networks[e.Network()]; ok {
					if endpoint.NetworkID != n.ID {
						return n, fmt.Errorf("待清理容器的网络身份已改变")
					}
					attached = true
				}
			}
			if attached {
				if _, err = h.docker("network", "disconnect", n.ID, e.Container); err != nil {
					return n, err
				}
			}
			_, err = h.docker("network", "rm", n.ID)
		}
		if !remove && h.state.NetworkIDs[e.Container] == "" {
			if h.state.NetworkIDs == nil {
				h.state.NetworkIDs = map[string]string{}
			}
			h.state.NetworkIDs[e.Container] = n.ID
			err = h.save()
		}
		return n, err
	}
	if remove {
		if _, err := os.Stat("/sys/class/net/" + e.Bridge()); !os.IsNotExist(err) {
			return network{}, fmt.Errorf("网络已消失但接口 %s 未完成清理", e.Bridge())
		}
		return network{}, nil
	}
	if _, err := os.Stat("/sys/class/net/" + e.Bridge()); err == nil || !os.IsNotExist(err) {
		return network{}, fmt.Errorf("宿主接口已被占用: %s", e.Bridge())
	}
	id, err := h.docker("network", "create", "--driver", "bridge", "--subnet", e.Subnet().String(), "--gateway", e.Subnet().Addr().Next().String(), "--opt", "com.docker.network.bridge.gateway_mode_ipv4=routed", "--opt", "com.docker.network.bridge.enable_icc=false", "--opt", "com.docker.network.bridge.name="+e.Bridge(), "--label", ownerLabel+"="+h.install.Worker, "--label", idLabel+"="+e.Container, e.Network())
	if err != nil {
		return network{}, err
	}
	n := network{ID: strings.TrimSpace(id)}
	if h.state.NetworkIDs == nil {
		h.state.NetworkIDs = map[string]string{}
	}
	h.state.NetworkIDs[e.Container] = n.ID
	return n, h.save()
}
func gatewayPriority(c inspect, e link.Endpoint) (int, error) {
	original, ok := c.NetworkSettings.Networks["bridge"]
	if !ok || original.IPAddress == "" {
		return 0, fmt.Errorf("缺少有效原 bridge 默认网关")
	}
	lowest := original.GwPriority
	for name, ep := range c.NetworkSettings.Networks {
		if name != e.Network() && ep.GwPriority < lowest {
			lowest = ep.GwPriority
		}
	}
	if lowest <= math.MinInt32 {
		return 0, fmt.Errorf("原网关优先级已达下限，无法附加更低优先级的网络")
	}
	return lowest - 1, nil
}
func (h *host) attach(e link.Endpoint, pool netip.Prefix) (*os.File, string, error) {
	ns, c, err := h.runningNamespace(e.Container)
	if err != nil || ns == nil {
		return nil, "", err
	}
	fail := func(err error) (*os.File, string, error) { ns.Close(); return nil, "", err }
	priority, err := gatewayPriority(c, e)
	if err != nil {
		return fail(err)
	}
	baseline, err := h.run(ns, "", "ip", "-4", "route", "show", "default")
	if err != nil || strings.TrimSpace(baseline) == "" {
		return fail(fmt.Errorf("原默认路由不可用"))
	}
	if _, attached := c.NetworkSettings.Networks[e.Network()]; !attached {
		if err = h.namespaceConflicts(ns, e, pool, ""); err != nil {
			return fail(err)
		}
	}
	n, err := h.endpointNetwork(e, false)
	if err != nil {
		return fail(err)
	}
	a, attached := c.NetworkSettings.Networks[e.Network()]
	if !attached {
		if _, err = h.docker("network", "connect", "--gw-priority", strconv.Itoa(priority), "--ip", e.IP.String(), n.ID, e.Container); err != nil {
			return fail(err)
		}
		c, err = h.inspect(e.Container)
		if err != nil {
			return fail(err)
		}
		a = c.NetworkSettings.Networks[e.Network()]
	}
	if a.IPAddress != e.IP.String() || a.NetworkID != n.ID || a.GwPriority != priority {
		return fail(fmt.Errorf("专用网络 ID、地址或网关优先级不匹配"))
	}
	after, err := h.run(ns, "", "ip", "-4", "route", "show", "default")
	if err != nil || after != baseline {
		return fail(fmt.Errorf("附加网络后的默认路由异常"))
	}
	out, err := h.run(ns, "", "ip", "-j", "-4", "addr")
	if err != nil {
		return fail(err)
	}
	var ifs []struct {
		Ifname string
		Addrs  []struct{ Local string } `json:"addr_info"`
	}
	if err = json.Unmarshal([]byte(out), &ifs); err != nil {
		return fail(err)
	}
	dev := ""
	for _, i := range ifs {
		for _, a := range i.Addrs {
			if a.Local == e.IP.String() {
				dev = i.Ifname
			}
		}
	}
	if !interfaceName.MatchString(dev) {
		return fail(fmt.Errorf("找不到专用容器接口"))
	}
	if err = h.namespaceConflicts(ns, e, pool, dev); err != nil {
		return fail(err)
	}
	return ns, dev, nil
}
func (h *host) namespaceConflicts(ns *os.File, e link.Endpoint, pool netip.Prefix, dev string) error {
	rs, err := h.routes(ns, "table", "all")
	if err != nil {
		return err
	}
	for _, r := range rs {
		p, er := routePrefix(r.Dst)
		if er != nil || !pool.Overlaps(p) {
			continue
		}
		if r.Dev == dev && e.Subnet().Contains(p.Addr()) && p.Bits() >= 30 {
			continue
		}
		if slices.ContainsFunc(h.state.Routes, func(o ownedRoute) bool { return o.Container == e.Container && r.owned(o) }) {
			continue
		}
		return fmt.Errorf("容器已有路由 %s dev %s 与互联地址池冲突", p, r.Dev)
	}
	return nil
}
func (h *host) direct(nic string, peer netip.Addr) error {
	s, err := h.run(nil, "", "ip", "-j", "-4", "route", "get", peer.String())
	if err != nil {
		return err
	}
	var rows []route
	if json.Unmarshal([]byte(s), &rows) != nil || len(rows) != 1 || rows[0].Dev != nic || rows[0].Gateway != "" {
		return fmt.Errorf("对端 %s 不是 %s 的直接下一跳", peer, nic)
	}
	return nil
}
func (h *host) sourceMACs(c link.Config, nic string) (map[link.Ref]string, error) {
	result := map[link.Ref]string{}
	peers := map[netip.Addr]string{}
	for _, e := range c.Endpoints {
		var value string
		if e.Node == c.Node {
			ins, err := h.inspect(e.Container)
			if err != nil {
				return nil, err
			}
			value = ins.NetworkSettings.Networks[e.Network()].MacAddress
		} else if cached := peers[e.Internal]; cached != "" {
			value = cached
		} else {
			if err := h.direct(nic, e.Internal); err != nil {
				return nil, err
			}
			// Resolve the neighbor on this interface. ICMP may be filtered independently of ARP.
			read := func() string {
				s, err := h.run(nil, "", "ip", "-j", "neigh", "show", "to", e.Internal.String(), "dev", nic)
				var ns []struct {
					LLAddr string
					State  []string
				}
				if err != nil || json.Unmarshal([]byte(s), &ns) != nil || len(ns) != 1 || slices.Contains(ns[0].State, "FAILED") || slices.Contains(ns[0].State, "INCOMPLETE") {
					return ""
				}
				return ns[0].LLAddr
			}
			value = read()
			if value == "" {
				_, _ = h.run(nil, "", "ping", "-n", "-c", "1", "-W", "1", "-I", nic, e.Internal.String())
				value = read()
			}
			peers[e.Internal] = value
		}
		mac, err := net.ParseMAC(value)
		if err != nil || len(mac) != 6 {
			return nil, fmt.Errorf("无法核实端点 %s 的二层来源", e.IP)
		}
		result[e.Ref] = mac.String()
	}
	return result, nil
}
func (h *host) route(ns *os.File, r ownedRoute, remove bool) error {
	rows, err := h.routes(ns, "exact", r.Dest+"/32")
	if err != nil {
		return err
	}
	for _, existing := range rows {
		if !existing.owned(r) {
			return fmt.Errorf("已有路由 %s 归属不符", r.Dest)
		}
		if !remove {
			return nil
		}
	}
	if remove && len(rows) == 0 {
		return nil
	}
	action := "add"
	if remove {
		action = "del"
	}
	args := []string{"-4", "route", action, r.Dest + "/32", "via", r.Gateway, "dev", r.Dev, "proto", "242"}
	if r.Source != "" {
		args = append(args, "src", r.Source)
	}
	_, err = h.run(ns, "", "ip", args...)
	return err
}

func (h *host) apply(c link.Config, nic string, keep *firewallPolicy) error {
	// Journal reservations before any external side effect. A retry sees resources
	// from both the previous configuration and an interrupted operation.
	h.state.Pending = c
	if err := h.save(); err != nil {
		return err
	}
	if len(c.Local()) > 0 {
		if err := h.conflicts(c.Pool); err != nil {
			return err
		}
	}
	for _, e := range c.Local() {
		if err := h.verifyRecord(e); err != nil {
			return err
		}
		for _, old := range h.state.Endpoints {
			if (old.Ref == e.Ref) != (old.IP == e.IP) {
				return fmt.Errorf("请先退出原端点再分配新地址")
			}
		}
		if !slices.ContainsFunc(h.state.Endpoints, func(old link.Endpoint) bool { return old.Ref == e.Ref && old.IP == e.IP }) {
			nets, err := h.networks()
			if err != nil {
				return err
			}
			if slices.ContainsFunc(nets, func(n network) bool {
				return n.Name == e.Network() || n.Options["com.docker.network.bridge.name"] == e.Bridge()
			}) {
				return fmt.Errorf("专用网络或接口名称已被占用: %s", e.Network())
			}
			if _, err = os.Stat("/sys/class/net/" + e.Bridge()); !os.IsNotExist(err) {
				return fmt.Errorf("专用接口名称已被占用: %s", e.Bridge())
			}
			h.state.Endpoints = append(h.state.Endpoints, e)
		}
	}
	if err := h.save(); err != nil {
		return err
	}
	if err := h.firewall(keep, false); err != nil {
		return err
	}
	if err := h.firewall(keep, true); err != nil {
		return err
	}
	spaces := map[string]*os.File{}
	defer func() {
		for _, f := range spaces {
			f.Close()
		}
	}()
	devs := map[string]string{}
	byRef := map[link.Ref]link.Endpoint{}
	for _, e := range c.Endpoints {
		byRef[e.Ref] = e
		if e.Node != c.Node {
			continue
		}
		ns, dev, err := h.attach(e, c.Pool)
		if err != nil {
			return err
		}
		if ns != nil {
			spaces[e.Container] = ns
			devs[e.Container] = dev
		}
	}
	live := liveConfig(c, func(e link.Endpoint) bool { return spaces[e.Container] != nil })
	var desired []ownedRoute
	add := func(r ownedRoute) {
		if !slices.Contains(desired, r) {
			desired = append(desired, r)
		}
	}
	for _, l := range live.Connections {
		for _, pair := range [][2]link.Ref{{l.A, l.B}, {l.B, l.A}} {
			a, b := byRef[pair[0]], byRef[pair[1]]
			if a.Node != c.Node {
				continue
			}
			add(ownedRoute{a.Container, b.IP.String(), a.Subnet().Addr().Next().String(), devs[a.Container], a.IP.String()})
			if b.Node != c.Node {
				add(ownedRoute{"", b.IP.String(), b.Internal.String(), nic, ""})
			}
		}
	}
	for _, old := range h.state.Routes {
		if slices.Contains(desired, old) {
			continue
		}
		ns := spaces[old.Container]
		if old.Container != "" && ns == nil {
			// Stopping or disconnecting the container removes its namespace routes.
			continue
		}
		if err := h.route(ns, old, true); err != nil {
			return err
		}
	}
	for _, r := range desired {
		if !slices.Contains(h.state.Routes, r) {
			// A pre-existing identical foreign route is also a conflict.
			rows, err := h.routes(spaces[r.Container], "exact", r.Dest+"/32")
			if err != nil {
				return err
			}
			if len(rows) != 0 {
				return fmt.Errorf("目的地址 %s 已有路由", r.Dest)
			}
			h.state.Routes = append(h.state.Routes, r)
			if err = h.save(); err != nil {
				return err
			}
		}
		if err := h.route(spaces[r.Container], r, false); err != nil {
			return err
		}
	}
	for _, old := range h.state.Endpoints {
		if _, ok := byRef[old.Ref]; ok {
			continue
		}
		if _, err := h.endpointNetwork(old, true); err != nil {
			return err
		}
	}
	for _, e := range c.Local() {
		ns, _, err := h.runningNamespace(e.Container)
		if err != nil {
			return err
		}
		if ns == nil && spaces[e.Container] == nil {
			continue
		}
		if ns == nil || spaces[e.Container] == nil {
			if ns != nil {
				ns.Close()
			}
			return fmt.Errorf("准备过程中容器运行状态已改变")
		}
		a, x := ns.Stat()
		b, y := spaces[e.Container].Stat()
		ns.Close()
		if x != nil || y != nil || !os.SameFile(a, b) {
			return fmt.Errorf("准备过程中容器 namespace 已改变")
		}
	}
	macs, err := h.sourceMACs(live, nic)
	if err != nil {
		return err
	}
	lines := policy(live, nic, macs)
	for _, old := range h.state.Endpoints {
		if _, keep := byRef[old.Ref]; !keep {
			delete(h.state.NetworkIDs, old.Container)
		}
	}
	h.state.Endpoints = c.Local()
	if err = h.firewall(lines, true); err != nil {
		return err
	}
	h.state.Routes = desired
	h.state.Config = c
	h.state.Revision = c.Revision
	if err := h.save(); err != nil {
		return err
	}
	if err = h.firewall(lines, false); err != nil {
		return err
	}
	h.state.FirewallHash, err = h.firewallDigest()
	if err != nil {
		_ = h.firewall(nil, false)
		return err
	}
	h.state.Sources = map[string]string{}
	for _, e := range live.Endpoints {
		h.state.Sources[e.IP.String()] = macs[e.Ref]
	}
	h.state.Guarded = false
	if err = h.save(); err != nil {
		_ = h.firewall(nil, false)
		return err
	}
	return nil
}

// Keep every local reservation in the ledger and deny rules, but only install
// routes and grants for running local endpoints and their connected peers.
func liveConfig(c link.Config, running func(link.Endpoint) bool) link.Config {
	live := c
	live.Endpoints = nil
	live.Connections = nil
	active := make(map[link.Ref]bool)
	for _, e := range c.Local() {
		if running(e) {
			active[e.Ref] = true
			live.Endpoints = append(live.Endpoints, e)
		}
	}
	for _, l := range c.Connections {
		if !l.Enabled || l.A.Node == c.Node && !active[l.A] || l.B.Node == c.Node && !active[l.B] {
			continue
		}
		live.Connections = append(live.Connections, l)
		active[l.A] = true
		active[l.B] = true
	}
	for _, e := range c.Endpoints {
		if e.Node != c.Node && active[e.Ref] {
			live.Endpoints = append(live.Endpoints, e)
		}
	}
	return live
}
func (h *host) probe(p link.Probe, c link.Config) error {
	if c.Revision != h.state.Revision || p.Port == 0 || p.From.Node != h.install.Worker {
		return fmt.Errorf("请先应用当前配置，再指定 TCP 目的端口诊断")
	}
	cfg := h.state.Config
	var a, b link.Endpoint
	for _, e := range cfg.Endpoints {
		if e.Ref == p.From {
			a = e
		}
		if e.Ref == p.To {
			b = e
		}
	}
	if !a.Valid() || !b.Valid() || !slices.ContainsFunc(cfg.Connections, func(l link.Connection) bool {
		return l.Enabled && ((l.A == a.Ref && l.B == b.Ref) || (l.B == a.Ref && l.A == b.Ref))
	}) {
		return fmt.Errorf("诊断需要已启用连接的两个端点")
	}
	ns, _, err := h.runningNamespace(a.Container)
	if err != nil {
		return err
	}
	if ns == nil {
		return fmt.Errorf("源容器已停止")
	}
	defer ns.Close()
	_, err = h.run(ns, "", "nc", "-n", "-z", "-w", "3", "-s", a.IP.String(), b.IP.String(), strconv.Itoa(int(p.Port)))
	return err
}

// observed checks the persisted attachments and /32 routes without changing them.
func (h *host) observed() error {
	nets, err := h.networks()
	if err != nil {
		return err
	}
	spaces := map[string]*os.File{}
	defer func() {
		for _, f := range spaces {
			f.Close()
		}
	}()
	for _, e := range h.state.Endpoints {
		if err = h.verifyRecord(e); err != nil {
			return err
		}
		ns, c, err := h.runningNamespace(e.Container)
		if err != nil {
			return err
		}
		if ns == nil {
			if _, active := h.state.Sources[e.IP.String()]; active {
				return fmt.Errorf("端点 %s 已停止，等待撤销运行时规则", e.Name)
			}
			continue
		}
		spaces[e.Container] = ns
		priority, priorityErr := gatewayPriority(c, e)
		a := c.NetworkSettings.Networks[e.Network()]
		if priorityErr != nil || a.IPAddress != e.IP.String() || a.GwPriority != priority || !slices.ContainsFunc(nets, func(n network) bool {
			return n.ID == a.NetworkID && h.owned(n, e) && len(n.Containers) == 1 && n.Containers[e.Container].IPv4Address == e.IP.String()+"/30"
		}) {
			return fmt.Errorf("端点 %s 专用网络已改变，请重试", e.Name)
		}
	}
	for _, r := range h.state.Routes {
		ns := spaces[r.Container]
		if r.Container != "" && ns == nil {
			return fmt.Errorf("端点的路由待清理")
		}
		rows, err := h.routes(ns, "exact", r.Dest+"/32")
		if err != nil {
			return err
		}
		if len(rows) != 1 || !rows[0].owned(r) {
			return fmt.Errorf("路由 %s 需要恢复", r.Dest)
		}
	}
	digest, err := h.firewallDigest()
	if err != nil {
		return err
	}
	if digest != h.state.FirewallHash {
		return fmt.Errorf("互联防火墙规则已改变，请重试")
	}
	nic, err := h.nic(h.state.Config.Internal)
	if err != nil {
		return err
	}
	live := liveConfig(h.state.Config, func(e link.Endpoint) bool { return spaces[e.Container] != nil })
	macs, err := h.sourceMACs(live, nic)
	if err != nil {
		return err
	}
	for _, e := range live.Endpoints {
		if h.state.Sources[e.IP.String()] != macs[e.Ref] {
			return fmt.Errorf("端点 %s 的二层来源已改变，请重试", e.IP)
		}
	}

	return nil
}
