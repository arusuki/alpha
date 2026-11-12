package linkhost

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	link "project-alpha/internal/interconnect"
)

var interfaceName = regexp.MustCompile("^[[:alnum:]_.-]{1,15}$")

type firewallPolicy struct {
	forward []string
	output  []string
}

func (p *firewallPolicy) rules(parent string) []string {
	if p != nil {
		switch parent {
		case "FORWARD", "DOCKER-USER":
			return p.forward
		case "OUTPUT":
			return p.output
		}
	}
	return nil
}

// Replies and fragmentation-needed errors must belong to a direction that is
// still authorized. Recompile all three paths when a rule changes, including
// established flows; an old conntrack entry alone must never preserve a grant.
func policy(c link.Config, nic string, macs map[link.Ref]string) *firewallPolicy {
	endpoints := map[link.Ref]link.Endpoint{}
	for _, e := range c.Endpoints {
		endpoints[e.Ref] = e
	}
	allow := &firewallPolicy{}
	for _, l := range c.Connections {
		if !l.Enabled {
			continue
		}
		for _, r := range l.Rules {
			if !r.Enabled {
				continue
			}
			directions := [][2]link.Ref{}
			if r.Direction != "ba" {
				directions = append(directions, [2]link.Ref{l.A, l.B})
			}
			if r.Direction != "ab" {
				directions = append(directions, [2]link.Ref{l.B, l.A})
			}
			for _, d := range directions {
				a, b := endpoints[d[0]], endpoints[d[1]]
				i, o := nic, nic
				if a.Node == c.Node {
					i = a.Bridge()
				}
				if b.Node == c.Node {
					o = b.Bridge()
				}
				protocol, orig, reply, ctProtocol, ctPort := "", "", "", "", ""
				if r.Protocol != "all" {
					protocol = " -p " + r.Protocol
					ctProtocol = " --ctproto " + r.Protocol
				}
				if r.First != 0 {
					p := strconv.Itoa(int(r.First))
					if r.Last != r.First {
						p += ":" + strconv.Itoa(int(r.Last))
					}
					orig = " --dport " + p
					reply = " --sport " + p
					ctPort = " --ctorigdstport " + p
				}
				flow := func(in, out string, s, t link.Endpoint, ports, state, dir string) string {
					return fmt.Sprintf("-i %s -o %s -s %s -d %s%s%s -m mac --mac-source %s -m conntrack --ctdir %s --ctstate %s", in, out, s.IP, t.IP, protocol, ports, macs[s.Ref], dir, state)
				}
				allow.forward = append(allow.forward, flow(i, o, a, b, orig, "NEW,ESTABLISHED", "ORIGINAL"), flow(o, i, b, a, reply, "ESTABLISHED", "REPLY"))
				errors := func(in, out string, source, target link.Endpoint, dir string) {
					// ICMP errors run opposite to the quoted packet. The outer
					// source can be a router, so match its admitted ingress MAC
					// and the connection's original tuple instead of its IP.
					related := fmt.Sprintf("-d %s -p icmp --icmp-type fragmentation-needed -m conntrack --ctstate RELATED --ctdir %s --ctorigsrc %s --ctorigdst %s%s%s", target.IP, dir, a.IP, b.IP, ctProtocol, ctPort)
					allow.forward = append(allow.forward, fmt.Sprintf("-i %s -o %s -m mac --mac-source %s %s", in, out, macs[source.Ref], related))
					if target.Node == c.Node {
						// Locally generated errors have no ingress interface or MAC.
						allow.output = append(allow.output, "-o "+out+" "+related)
					}
				}
				errors(o, i, b, a, "REPLY")
				errors(i, o, a, b, "ORIGINAL")
			}
		}
	}
	if len(allow.forward) == 0 {
		return nil
	}
	return allow
}

type chain struct{ binary, table, parent, name string }

var chains = []chain{
	{"iptables", "mangle", "FORWARD", "ALPHA-LINK-GUARD"},
	{"iptables", "mangle", "INPUT", "ALPHA-LINK-HOST"},
	{"iptables", "mangle", "OUTPUT", "ALPHA-LINK-OUT"},
	{"ip6tables", "mangle", "FORWARD", "ALPHA-LINK-GUARD"},
	{"ip6tables", "mangle", "INPUT", "ALPHA-LINK-HOST"},
	{"ip6tables", "mangle", "OUTPUT", "ALPHA-LINK-OUT"},
	{"iptables", "filter", "DOCKER-USER", "ALPHA-LINK-ACCESS"},
}

func (h *host) chain(ch chain, lines []string, remove bool) error {
	args := []string{"-w", "5", "-t", ch.table, "-S"}
	listing, err := h.run(nil, "", ch.binary, args...)
	if err != nil {
		return err
	}
	rules := strings.Split(strings.ReplaceAll(strings.TrimSpace(listing), "\"", ""), "\n")
	marker := "-m comment --comment alpha-link-" + h.install.Worker
	owned := "-A " + ch.name + " " + marker + " -j RETURN"
	jump := "-A " + ch.parent + " " + marker + " -j " + ch.name
	exists := slices.Contains(rules, "-N "+ch.name)
	if exists && !slices.Contains(rules, owned) {
		return fmt.Errorf("防火墙链 %s 已被占用", ch.name)
	}
	var edits []string
	if exists {
		edits = append(edits, "-F "+ch.name)
	}
	for _, line := range rules {
		if line == jump {
			edits = append(edits, "-D "+strings.TrimPrefix(jump, "-A "))
		}
	}
	if remove {
		if exists {
			edits = append(edits, "-X "+ch.name)
		}
	} else {
		if !exists {
			edits = append(edits, ":"+ch.name+" - [0:0]")
		}
		for _, line := range lines {
			edits = append(edits, "-A "+ch.name+" "+line)
		}
		edits = append(edits, owned, "-I "+ch.parent+" 1 "+marker+" -j "+ch.name)
	}
	if len(edits) == 0 {
		return nil
	}
	_, err = h.run(nil, "*"+ch.table+"\n"+strings.Join(edits, "\n")+"\nCOMMIT\n", ch.binary+"-restore", "--wait", "5", "--noflush")
	return err
}
func (h *host) firewall(allow *firewallPolicy, access bool) error {
	bridges := make([]string, 0, len(h.state.Endpoints))
	for _, e := range h.state.Endpoints {
		bridges = append(bridges, e.Bridge())
	}
	slices.Sort(bridges)
	order := slices.Clone(chains)
	// Close the IPv4 forwarding gate first, reopen it last after every guard is installed.
	if allow != nil && !access {
		order = append(order[1:], chains[0])
	}
	for _, ch := range order {
		if (ch.name == "ALPHA-LINK-ACCESS") != access {
			continue
		}
		var lines []string
		if ch.binary == "iptables" {
			for _, rule := range allow.rules(ch.parent) {
				target := "RETURN"
				if access {
					target = "ACCEPT"
				}
				lines = append(lines, rule+" -j "+target)
			}
		}
		if ch.binary == "iptables" && (ch.parent == "FORWARD" || access) {
			for _, e := range h.state.Endpoints {
				lines = append(lines, "-s "+e.IP.String()+" -j DROP", "-d "+e.IP.String()+" -j DROP")
			}
		}
		for _, br := range bridges {
			if ch.parent != "OUTPUT" {
				lines = append(lines, "-i "+br+" -j DROP")
			}
			if ch.parent != "INPUT" {
				lines = append(lines, "-o "+br+" -j DROP")
			}
		}
		if err := h.chain(ch, lines, false); err != nil {
			return err
		}
	}
	return nil
}

// Snapshot only our chains and the precedence of our own jumps. Other Docker or
// site firewall changes must not invalidate a successfully applied policy.
func (h *host) firewallDigest() (string, error) {
	hash := sha256.New()
	tables := map[string][]string{}
	for _, ch := range chains {
		key := ch.binary + "/" + ch.table
		rows, ok := tables[key]
		if !ok {
			listing, err := h.run(nil, "", ch.binary, "-w", "5", "-t", ch.table, "-S")
			if err != nil {
				return "", err
			}
			rows = strings.Split(strings.ReplaceAll(listing, "\"", ""), "\n")
			tables[key] = rows
		}
		jump := "-A " + ch.parent + " -m comment --comment alpha-link-" + h.install.Worker + " -j " + ch.name
		first := slices.IndexFunc(rows, func(row string) bool { return strings.HasPrefix(row, "-A "+ch.parent+" ") })
		if first < 0 || rows[first] != jump {
			return "", fmt.Errorf("互联防火墙跳转 %s 已改变", ch.parent)
		}
		fmt.Fprintln(hash, key)
		for _, row := range rows {
			if strings.HasPrefix(row, "-A "+ch.name+" ") {
				fmt.Fprintln(hash, row)
			}
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
