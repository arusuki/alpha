package interconnect

import (
	"net/netip"
	"strings"
	"testing"
)

func TestAllocationsAndRuleValidation(t *testing.T) {
	for _, test := range []struct {
		cidr string
		ok   bool
	}{{"10.240.0.0/16", true}, {"100.64.0.0/16", true}, {"10.1.2.1/24", false}, {"10.1.2.0/25", false}, {"224.0.0.0/8", false}, {"::/64", false}} {
		if ValidPool(netip.MustParsePrefix(test.cidr)) != test.ok {
			t.Fatal(test.cidr)
		}
	}
	p := netip.MustParsePrefix("10.240.1.0/24")
	used := map[string]bool{}
	for range 64 {
		e, err := Allocate(p, 30, used)
		if err != nil || !p.Contains(e.Addr()) || used[e.String()] {
			t.Fatal(e, err)
		}
		used[e.String()] = true
	}
	if _, err := Allocate(p, 30, used); err == nil {
		t.Fatal("exhausted subnet accepted")
	}
	a := Ref{strings.Repeat("a", 32), strings.Repeat("1", 64)}
	b := Ref{strings.Repeat("b", 32), strings.Repeat("2", 64)}
	for _, test := range []struct {
		rule Rule
		ok   bool
	}{
		{Rule{"ab", "tcp", 0, 0, true}, true}, {Rule{"ba", "udp", 1, 65535, true}, true}, {Rule{"both", "icmp", 0, 0, true}, true}, {Rule{"both", "all", 0, 0, true}, true},
		{Rule{"both", "icmp", 22, 22, true}, false}, {Rule{"ab", "tcp", 0, 7, true}, false}, {Rule{"ab", "udp", 80, 79, false}, false}, {Rule{"invalid", "all", 0, 0, false}, false},
	} {
		c := Connection{ID: a.Node, A: a, B: b, Rules: []Rule{test.rule}}
		if (c.Validate() == nil) != test.ok {
			t.Fatal(test.rule)
		}
	}
}
