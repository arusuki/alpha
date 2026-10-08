package mihomo

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const sampleSubscription = `proxies:
  - {name: 美国 A, type: vless, server: example.invalid, port: 443, uuid: sample, tls: true, reality-opts: {public-key: original, short-id: abcd}}
  - {name: 美国 0.1, type: ss, server: example.invalid, port: 443, cipher: aes-128-gcm, password: private-password}
  - {name: 日本 A, type: trojan, server: example.invalid, port: 443, password: private-password}
proxy-groups:
  - {name: ignored, type: select, proxies: [日本 A]}
external-controller: 0.0.0.0:9090
secret: ignored
`

func testConfig() Config {
	c := DefaultConfig()
	c.Subscriptions = []Subscription{{Name: "primary", Content: sampleSubscription}}
	c.Filters = []Filter{{Name: "美国", Include: "美国|US", Exclude: `0\.0?1`, Types: []string{"vless", "ss"}, Sources: []string{"primary"}}}
	return c
}
func TestSubscriptionFiltersTemplateAndProtocolPreservation(t *testing.T) {
	c := testConfig()
	c.Template = `mixed-port: {{ .MixedPort }}
proxies:
{{ yaml .Proxies | indent 2 }}
proxy-groups:
  - name: {{ quote .Node.Name }}
    type: select
    proxies:
{{ names "美国" | yaml | indent 6 }}
{{ if eq .Node.Role "worker" }}rules:
  - MATCH,工作节点
{{ end }}
external-controller: 0.0.0.0:9090
external-controller-tls: 0.0.0.0:9443
secret: untrusted
`
	proxies, err := Fetch(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(c, proxies, Target{ID: "worker", Name: "工作节点", Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if b.ProxyCount != 3 || !reflect.DeepEqual(b.Groups[0].Proxies, []string{"美国 A"}) {
		t.Fatalf("unexpected candidates: %+v", b.Groups)
	}
	if !strings.Contains(b.YAML, "public-key: original") || !strings.Contains(b.YAML, "MATCH,工作节点") || strings.Contains(b.YAML, "external-controller") || strings.Contains(b.YAML, "untrusted") {
		t.Fatalf("lost protocol or leaked controller: %s", b.YAML)
	}
	c.Filter = "美国"
	p, err := Fetch(context.Background(), c, nil)
	if err != nil || len(p) != 1 {
		t.Fatalf("global filter: %v %v", p, err)
	}
	c.Subscriptions = append(c.Subscriptions, Subscription{Name: "secondary", Content: sampleSubscription, Prefix: "second/"})
	c.Filter = ""
	p, err = Fetch(context.Background(), c, nil)
	if err != nil || len(p) != 6 {
		t.Fatalf("merge: %d %v", len(p), err)
	}
	c.Subscriptions[1].Prefix = ""
	if _, err = Fetch(context.Background(), c, nil); err == nil {
		t.Fatal("duplicate proxy silently accepted")
	}
}

func TestTemplateFailuresAreExplicit(t *testing.T) {
	c := testConfig()
	p, err := Fetch(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{
		`{{ .Missing }}`,
		`{{ names "missing" | yaml }}`,
		"proxies: []\nproxy-groups: [{name: X, type: select, proxies: []}]",
		"proxies: []\nproxy-groups: [{name: X, type: select, proxies: [missing]}]",
		"proxies: []\nproxy-groups: [{name: X, type: select, proxies: [Y]}, {name: Y, type: select, proxies: [X]}]",
		"proxies: []\nproxy-groups: [{name: X, type: select, proxies: [DIRECT]}]\nproxy-providers: {}",
		"proxies: []\nproxies: []\nproxy-groups: []",
		"proxies: []\n---\nproxy-groups: []",
	}
	for _, source := range cases {
		c.Template = source
		if _, err = Render(c, p, Target{}); err == nil {
			t.Errorf("accepted invalid template %s", source)
		}
	}
	c = testConfig()
	c.Filters[0].Include = "["
	if err = c.Validate(); err == nil {
		t.Fatal("invalid regex accepted")
	}
	c = testConfig()
	c.Filters[0].Sources = []string{"missing"}
	if err = c.Validate(); err == nil {
		t.Fatal("unknown source accepted")
	}
}

func TestFetchAtomicAndCredentialSafeErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			fmt.Fprint(w, sampleSubscription)
		} else {
			http.Error(w, "token=super-secret", 403)
		}
	}))
	defer s.Close()
	c := DefaultConfig()
	c.Subscriptions = []Subscription{{Name: "one", URL: s.URL + "/ok"}, {Name: "two", URL: s.URL + "/bad?token=super-secret"}}
	p, err := Fetch(context.Background(), c, nil)
	if p != nil || err == nil || !strings.Contains(err.Error(), "two") || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("partial fetch or credential leak: %v %v", p, err)
	}
	c.Subscriptions = c.Subscriptions[1:]
	c.Subscriptions[0].URL = "http://127.0.0.1:1/?secret=super-secret"
	if _, err = Fetch(context.Background(), c, nil); err == nil || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("URL exposed: %v", err)
	}
}

func TestURISubscriptions(t *testing.T) {
	uris := []string{
		"ss://" + base64.RawStdEncoding.EncodeToString([]byte("aes-128-gcm:p:a:ss")) + "@example.invalid:443#US",
		"vless://user@example.invalid:443?security=reality&pbk=public&sid=ab&fp=chrome&type=ws&path=%2Fsocket&sni=example.invalid#US",
		"trojan://password@example.invalid:443?sni=example.invalid#US",
		"hy2://password@example.invalid:443?obfs=salamander&obfs-password=test#US",
		"vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"v":"2","ps":"US","add":"example.invalid","port":"443","id":"uuid","aid":"0","net":"ws","path":"/socket","tls":"tls"}`)),
	}
	for _, uri := range uris {
		for _, raw := range []string{uri, base64.StdEncoding.EncodeToString([]byte(uri))} {
			p, err := ParseSubscription([]byte(raw))
			if err != nil || len(p) != 1 || p[0]["name"] != "US" {
				t.Fatalf("parse %s: %v %v", uri, p, err)
			}
		}
	}
	if _, err := ParseSubscription([]byte("trojan://pass@example.invalid:443?unknown=yes#US")); err == nil {
		t.Fatal("unknown URI option silently dropped")
	}
	for _, uri := range []string{"ss://aes-128-gcm:pass@example.invalid:443?security=tls", "vless://user@example.invalid:443?encryption=unsupported", "trojan://pass@example.invalid:443?insecure=unexpected"} {
		if _, err := ParseSubscription([]byte(uri)); err == nil {
			t.Fatalf("ignored unsupported option in %s", uri)
		}
	}
	p, err := ParseSubscription([]byte("ss://aes-128-gcm:p+a%3Ass@example.invalid:443#plus"))
	if err != nil || p[0]["password"] != "p+a:ss" {
		t.Fatalf("SS password corrupted: %v %v", p, err)
	}
}
