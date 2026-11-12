package linkhost

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	link "project-alpha/internal/interconnect"
)

func TestPolicyPMTU(t *testing.T) {
	a := link.Endpoint{Ref: link.Ref{Node: "a", Container: "one"}, IP: netip.MustParseAddr("10.240.0.2")}
	b := link.Endpoint{Ref: link.Ref{Node: "b", Container: "two"}, IP: netip.MustParseAddr("10.240.1.2")}
	macs := map[link.Ref]string{a.Ref: "02:00:00:00:00:01", b.Ref: "02:00:00:00:00:02"}
	for _, tc := range []struct {
		name, direction, protocol, node, ctPort string
		first, last                             uint16
	}{
		{"TCP range from local", "ab", "tcp", "a", " --ctorigdstport 18000:18001", 18000, 18001},
		{"UDP port to local", "ab", "udp", "b", " --ctorigdstport 53", 53, 53},
		{"reverse TCP", "ba", "tcp", "a", " --ctorigdstport 443", 443, 443},
		{"ICMP", "ab", "icmp", "a", "", 0, 0},
		{"all", "ab", "all", "a", "", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := link.Config{Node: tc.node, Endpoints: []link.Endpoint{a, b}, Connections: []link.Connection{{
				A: a.Ref, B: b.Ref, Enabled: true,
				Rules: []link.Rule{{Direction: tc.direction, Protocol: tc.protocol, First: tc.first, Last: tc.last, Enabled: true}},
			}}}
			p := policy(c, "lan0", macs)
			if p == nil || len(p.forward) != 4 || len(p.output) != 1 {
				t.Fatalf("missing data or PMTU paths: %#v", p)
			}
			source, target := a, b
			if tc.direction == "ba" {
				source, target = b, a
			}
			in, out := "lan0", "lan0"
			if source.Node == c.Node {
				in = source.Bridge()
			}
			if target.Node == c.Node {
				out = target.Bridge()
			}
			if !strings.Contains(p.forward[0], "--ctdir ORIGINAL --ctstate NEW,ESTABLISHED") || !strings.Contains(p.forward[1], "--ctdir REPLY --ctstate ESTABLISHED") {
				t.Fatalf("data direction changed: %v", p.forward[:2])
			}
			for i, expectation := range []struct {
				in, out, mac, destination, direction string
			}{
				{out, in, macs[target.Ref], source.IP.String(), "REPLY"},
				{in, out, macs[source.Ref], target.IP.String(), "ORIGINAL"},
			} {
				rule := p.forward[2+i]
				for _, part := range []string{
					"-i " + expectation.in + " -o " + expectation.out,
					"--mac-source " + expectation.mac,
					"-d " + expectation.destination,
					"-p icmp --icmp-type fragmentation-needed",
					"--ctstate RELATED --ctdir " + expectation.direction,
					"--ctorigsrc " + source.IP.String() + " --ctorigdst " + target.IP.String(),
					tc.ctPort,
				} {
					if !strings.Contains(rule, part) {
						t.Errorf("PMTU rule %q lacks %q", rule, part)
					}
				}
				if tc.protocol != "all" && !strings.Contains(rule, "--ctproto "+tc.protocol) {
					t.Errorf("PMTU rule lacks original protocol: %s", rule)
				}
			}
			local := a
			if c.Node == b.Node {
				local = b
			}
			if !strings.HasPrefix(p.output[0], "-o "+local.Bridge()+" -d "+local.IP.String()+" ") || strings.Contains(p.output[0], "-i ") || strings.Contains(p.output[0], "--mac-source") {
				t.Fatalf("invalid local PMTU path: %s", p.output[0])
			}
			for _, parent := range []string{"FORWARD", "DOCKER-USER"} {
				if !slices.Equal(p.rules(parent), p.forward) {
					t.Errorf("missing forwarded PMTU rules in %s", parent)
				}
			}
			if !slices.Equal(p.rules("OUTPUT"), p.output) || p.rules("INPUT") != nil {
				t.Fatal("PMTU rules assigned to the wrong host chain")
			}
		})
	}
}

func TestPolicyPMTURevocation(t *testing.T) {
	a := link.Endpoint{Ref: link.Ref{Node: "a", Container: "one"}, IP: netip.MustParseAddr("10.240.0.2")}
	b := link.Endpoint{Ref: link.Ref{Node: "a", Container: "two"}, IP: netip.MustParseAddr("10.240.0.6")}
	c := link.Config{Node: "a", Endpoints: []link.Endpoint{a, b}, Connections: []link.Connection{{
		A: a.Ref, B: b.Ref, Enabled: true,
		Rules: []link.Rule{{Direction: "both", Protocol: "tcp", First: 80, Last: 81, Enabled: true}},
	}}}
	macs := map[link.Ref]string{a.Ref: "02:00:00:00:00:01", b.Ref: "02:00:00:00:00:02"}
	p := policy(c, "lan0", macs)
	if len(p.output) != 4 || len(p.forward) != 8 {
		t.Fatalf("both local directions need PMTU feedback: %#v", p)
	}
	c.Connections[0].Rules[0] = link.Rule{Direction: "ba", Protocol: "tcp", First: 81, Last: 81, Enabled: true}
	p = policy(c, "lan0", macs)
	if len(p.output) != 2 || len(p.forward) != 4 {
		t.Fatalf("narrowed grant retained removed directions: %#v", p)
	}
	for _, rule := range append(slices.Clone(p.forward[2:]), p.output...) {
		if !strings.Contains(rule, "--ctorigsrc "+b.IP.String()+" --ctorigdst "+a.IP.String()+" --ctproto tcp --ctorigdstport 81") {
			t.Errorf("PMTU retained a removed port or direction: %s", rule)
		}
	}
	c.Connections[0].Rules[0].Enabled = false
	if policy(c, "lan0", macs) != nil {
		t.Fatal("disabled rule retained a grant")
	}
	c.Connections[0].Rules[0].Enabled = true
	c.Connections[0].Enabled = false
	if policy(c, "lan0", macs) != nil {
		t.Fatal("disabled connection retained a grant")
	}
	var closed *firewallPolicy
	for _, parent := range []string{"FORWARD", "DOCKER-USER", "OUTPUT", "INPUT"} {
		if closed.rules(parent) != nil {
			t.Errorf("default-deny guard retained %s grants", parent)
		}
	}
}
