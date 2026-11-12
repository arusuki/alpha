// Package interconnect defines the declarative policy shared by control, worker and helper.
package interconnect

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
)

const Protocol = 1
const WorkerPath = "/api/containers/interconnect"
const ControlPath = "/api/cluster/interconnect"
const Helper = "/usr/local/libexec/project-alpha/alpha-interconnect-helper"

var nodeID = regexp.MustCompile("^[a-f0-9]{32}$")
var containerID = regexp.MustCompile("^[a-f0-9]{64}$")

type Ref struct {
	Node      string `json:"node"`
	Container string `json:"container"`
}

func (r Ref) Valid() bool { return nodeID.MatchString(r.Node) && containerID.MatchString(r.Container) }

type Endpoint struct {
	Ref
	IP       netip.Addr `json:"ip"`
	Name     string     `json:"name"`
	Owner    string     `json:"owner"`
	Internal netip.Addr `json:"internal_ip"`
	Removing bool       `json:"removing,omitempty"`
}

func (e Endpoint) Subnet() netip.Prefix { return netip.PrefixFrom(e.IP, 30).Masked() }
func (e Endpoint) Bridge() string       { return fmt.Sprintf("alb%x", e.IP.As4()) }
func (e Endpoint) Network() string      { return "alpha-link-" + e.Bridge() }

type Rule struct {
	Direction string `json:"direction"`
	Protocol  string `json:"protocol"`
	First     uint16 `json:"first"`
	Last      uint16 `json:"last"`
	Enabled   bool   `json:"enabled"`
}
type Connection struct {
	ID      string `json:"id"`
	A       Ref    `json:"a"`
	B       Ref    `json:"b"`
	Enabled bool   `json:"enabled"`
	Rules   []Rule `json:"rules"`
}

func (c Connection) Validate() error {
	if !nodeID.MatchString(c.ID) || !c.A.Valid() || !c.B.Valid() || c.A == c.B || c.Rules == nil || len(c.Rules) > 128 {
		return fmt.Errorf("连接标识、端点或规则数量无效")
	}
	for _, r := range c.Rules {
		if !slices.Contains([]string{"ab", "ba", "both"}, r.Direction) || !slices.Contains([]string{"tcp", "udp", "icmp", "all"}, r.Protocol) {
			return fmt.Errorf("方向或协议无效")
		}
		if r.Last < r.First || (r.First == 0 && r.Last != 0) || ((r.Protocol != "tcp" && r.Protocol != "udp") && r.First != 0) {
			return fmt.Errorf("目的端口应为 1–65535 的闭区间或全部端口")
		}
	}
	return nil
}
func ValidPool(p netip.Prefix) bool {
	if !p.Addr().Is4() || p != p.Masked() || p.Bits() > 24 {
		return false
	}
	for _, block := range []string{"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "240.0.0.0/4"} {
		if p.Overlaps(netip.MustParsePrefix(block)) {
			return false
		}
	}
	return true
}
func Offset(addr netip.Addr, n uint32) netip.Addr {
	b := addr.As4()
	binary.BigEndian.PutUint32(b[:], binary.BigEndian.Uint32(b[:])+n)
	return netip.AddrFrom4(b)
}
func Allocate(pool netip.Prefix, bits int, used map[string]bool) (netip.Prefix, error) {
	for n := uint32(0); n < 1<<(32-pool.Bits()); n += 1 << (32 - bits) {
		p := netip.PrefixFrom(Offset(pool.Addr(), n), bits)
		if !used[p.String()] {
			return p, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("地址池 %s 容量不足", pool)
}

type Config struct {
	Protocol    int          `json:"protocol"`
	Node        string       `json:"node"`
	Revision    int64        `json:"revision"`
	Pool        netip.Prefix `json:"pool"`
	Subnet      netip.Prefix `json:"subnet"`
	Internal    netip.Addr   `json:"internal_ip"`
	Endpoints   []Endpoint   `json:"endpoints"`
	Connections []Connection `json:"connections"`
}

func (c Config) Validate() error {
	if c.Protocol != Protocol || !nodeID.MatchString(c.Node) || c.Revision < 1 {
		return fmt.Errorf("互联协议、节点身份或版本无效")
	}
	if !ValidPool(c.Pool) {
		return fmt.Errorf("互联地址池无效")
	}
	if len(c.Endpoints) > 0 && (!c.Internal.Is4() || !c.Internal.IsGlobalUnicast() || c.Pool.Contains(c.Internal)) {
		return fmt.Errorf("互联地址池或节点 IPv4 无效")
	}
	if c.Subnet.Bits() != 24 || c.Subnet != c.Subnet.Masked() || !c.Pool.Contains(c.Subnet.Addr()) {
		return fmt.Errorf("节点子网必须为地址池中的 /24")
	}

	refs := map[Ref]bool{}
	ips := map[netip.Addr]bool{}
	hops := map[string]netip.Addr{}
	for _, e := range c.Endpoints {
		if !e.Valid() || refs[e.Ref] || ips[e.IP] || e.Removing {
			return fmt.Errorf("互联端点身份或地址重复或无效")
		}
		if !c.Pool.Contains(e.IP) || e.IP != Offset(e.Subnet().Addr(), 2) {
			return fmt.Errorf("端点地址不属于专用 /30 分配")
		}
		if !e.Internal.Is4() || !e.Internal.IsGlobalUnicast() || c.Pool.Contains(e.Internal) {
			return fmt.Errorf("对端内网 IPv4 地址无效")
		}
		local := e.Node == c.Node
		if local != c.Subnet.Contains(e.IP) || local != (e.Internal == c.Internal) {
			return fmt.Errorf("地址分配与节点身份不符")
		}
		if prev, ok := hops[e.Node]; ok && prev != e.Internal {
			return fmt.Errorf("节点内网地址不一致")
		}
		hops[e.Node] = e.Internal
		refs[e.Ref] = true
		ips[e.IP] = true
	}
	for _, l := range c.Connections {
		if err := l.Validate(); err != nil {
			return err
		}
		if !refs[l.A] || !refs[l.B] || (l.A.Node != c.Node && l.B.Node != c.Node) {
			return fmt.Errorf("连接不属于此节点或缺少端点")
		}
	}
	return nil
}
func (c Config) Local() []Endpoint {
	var out []Endpoint
	for _, e := range c.Endpoints {
		if e.Node == c.Node {
			out = append(out, e)
		}
	}
	return out
}

type Probe struct {
	From Ref    `json:"from"`
	To   Ref    `json:"to"`
	Port uint16 `json:"port"`
}
type Request struct {
	Operation string `json:"operation"`
	Config    Config `json:"config"`
	Probe     Probe  `json:"probe,omitempty"`
	Container string `json:"container,omitempty"`
	Force     bool   `json:"force,omitempty"`
}
type Status struct {
	Protocol  int    `json:"protocol"`
	Interface string `json:"interface"`
	Daemon    string `json:"daemon"`
	Firewall  string `json:"firewall"`
	Error     string `json:"error,omitempty"`
	Applied   int64  `json:"applied"`
}
