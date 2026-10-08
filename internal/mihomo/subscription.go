package mihomo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var builtins = map[string]bool{"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true, "COMPATIBLE": true, "GLOBAL": true}

func decodeYAML(raw []byte, out any) error {
	if len(raw) > MaxSubscription {
		return fmt.Errorf("YAML 超过大小限制")
	}
	d := yaml.NewDecoder(bytes.NewReader(raw))
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("YAML 无效：%w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("仅支持单份 YAML 文档")
	}
	return nil
}

// Fetch reads all sources as one generation. A failed source never produces a
// partial replacement configuration. Subscription URLs never appear in errors.
func Fetch(ctx context.Context, c Config, client *http.Client) ([]Proxy, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || req.URL.User != nil || (req.URL.Scheme != "https" && req.URL.Scheme != "http") || (via[0].URL.Scheme == "https" && req.URL.Scheme != "https") {
				return fmt.Errorf("订阅重定向被拒绝")
			}
			return nil
		}}
	}
	out := []Proxy{}
	seen := map[string]bool{}
	for _, s := range c.Subscriptions {
		raw := []byte(s.Content)
		if s.URL != "" {
			callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			req, err := http.NewRequestWithContext(callCtx, "GET", s.URL, nil)
			if err != nil {
				cancel()
				return nil, fmt.Errorf("订阅 %s 请求无效", s.Name)
			}
			req.Header.Set("User-Agent", "clash.meta/project-alpha")
			resp, err := client.Do(req)
			if err != nil {
				cancel()
				return nil, fmt.Errorf("订阅 %s 下载失败，请检查网络和 URL", s.Name)
			}
			raw, err = io.ReadAll(io.LimitReader(resp.Body, MaxSubscription+1))
			resp.Body.Close()
			cancel()
			if err != nil || len(raw) > MaxSubscription {
				return nil, fmt.Errorf("订阅 %s 读取失败或超过 8 MiB", s.Name)
			}
			if resp.StatusCode != 200 {
				return nil, fmt.Errorf("订阅 %s 返回 HTTP %d", s.Name, resp.StatusCode)
			}
		}
		values, err := ParseSubscription(raw)
		if err != nil {
			return nil, fmt.Errorf("订阅 %s：%w", s.Name, err)
		}
		for _, v := range values {
			name, _ := v["name"].(string)
			name = s.Prefix + name
			if !validName(name) || builtins[name] || seen[name] {
				return nil, fmt.Errorf("代理名称无效、重复或为保留名称：%q；可设置订阅前缀", name)
			}
			seen[name] = true
			v["name"] = name
			// Keep protocol-specific fields intact, including TLS and transports.
			if peer, ok := v["dialer-proxy"].(string); ok && !builtins[peer] {
				v["dialer-proxy"] = s.Prefix + peer
			}
			out = append(out, Proxy{Value: v, Source: s.Name})
			if len(out) > MaxProxies {
				return nil, fmt.Errorf("订阅节点总数超过 %d", MaxProxies)
			}
		}
	}
	return c.filter(c.Filter, out)
}

func validName(name string) bool {
	return strings.TrimSpace(name) != "" && len(name) <= 256 && !strings.ContainsAny(name, "\x00\r\n")
}

func ParseSubscription(raw []byte) ([]map[string]any, error) {
	if len(raw) > MaxSubscription {
		return nil, fmt.Errorf("订阅超过 8 MiB")
	}
	raw = bytes.TrimPrefix(bytes.TrimSpace(raw), []byte{0xef, 0xbb, 0xbf})
	var source map[string]any
	if decodeYAML(raw, &source) == nil {
		list, ok := source["proxies"].([]any)
		if !ok || len(list) == 0 {
			return nil, fmt.Errorf("需要含非空 proxies 列表的 Clash/Mihomo YAML；不导入订阅的策略组和规则")
		}
		if len(list) > MaxProxies {
			return nil, fmt.Errorf("订阅节点过多")
		}
		out := make([]map[string]any, 0, len(list))
		for i, item := range list {
			p, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("第 %d 个节点不是对象", i+1)
			}
			name, _ := p["name"].(string)
			typ, _ := p["type"].(string)
			if !validName(name) || typ == "" {
				return nil, fmt.Errorf("第 %d 个节点缺少有效 name 或 type", i+1)
			}
			out = append(out, p)
		}
		return out, nil
	}
	text := string(raw)
	if !strings.Contains(text, "://") {
		decoded, err := decodeBase64(text)
		if err != nil {
			return nil, fmt.Errorf("需要 Clash/Mihomo YAML 或 URI/base64 订阅")
		}
		text = string(decoded)
	}
	out := []map[string]any{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := parseURI(line)
		if err != nil {
			return nil, fmt.Errorf("URI 第 %d 行：%w", i+1, err)
		}
		out = append(out, p)
		if len(out) > MaxProxies {
			return nil, fmt.Errorf("订阅节点过多")
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("订阅没有节点")
	}
	return out, nil
}

func decodeBase64(s string) ([]byte, error) {
	s = strings.Join(strings.Fields(s), "")
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := encoding.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("base64 无效")
}

// URI conversion deliberately rejects unsupported options instead of silently
// producing a weaker or non-working connection. YAML supports all core types.
func parseURI(raw string) (map[string]any, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("URI 无效")
	}
	if u.Scheme == "vmess" {
		b, err := decodeBase64(strings.TrimPrefix(raw, "vmess://"))
		if err != nil {
			return nil, err
		}
		var v struct {
			PS, Add, ID, Net, Host, Path, TLS, SNI, Scy string
			Port, Aid                                   json.RawMessage
		}
		if json.Unmarshal(b, &v) != nil {
			return nil, fmt.Errorf("VMess JSON 无效")
		}
		port, _ := strconv.Atoi(strings.Trim(string(v.Port), `"`))
		aid, _ := strconv.Atoi(strings.Trim(string(v.Aid), `"`))
		if port < 1 || port > 65535 || v.Add == "" || v.ID == "" {
			return nil, fmt.Errorf("VMess 地址或凭据缺失")
		}
		if v.Scy == "" {
			v.Scy = "auto"
		}
		if v.PS == "" {
			v.PS = v.Add
		}
		p := map[string]any{"name": v.PS, "type": "vmess", "server": v.Add, "port": port, "uuid": v.ID, "alterId": aid, "cipher": v.Scy, "tls": v.TLS == "tls", "udp": true}
		if v.SNI != "" {
			p["servername"] = v.SNI
		}
		if v.Net == "ws" {
			p["network"] = "ws"
			p["ws-opts"] = map[string]any{"path": v.Path, "headers": map[string]any{"Host": v.Host}}
		} else if v.Net != "" && v.Net != "tcp" {
			return nil, fmt.Errorf("VMess 此传输请使用 YAML 订阅")
		}
		return p, nil
	}
	if u.Scheme == "ss" && u.User == nil {
		body := strings.TrimPrefix(raw, "ss://")
		body, fragment, _ := strings.Cut(body, "#")
		b, e := decodeBase64(body)
		if e != nil {
			return nil, fmt.Errorf("SS URI 无效")
		}
		u, err = url.Parse("ss://" + string(b) + "#" + fragment)
		if err != nil {
			return nil, fmt.Errorf("SS URI 无效")
		}
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || u.Hostname() == "" || u.User == nil {
		return nil, fmt.Errorf("地址、端口或凭据缺失")
	}
	name := u.Fragment
	if name == "" {
		name = u.Hostname()
	}
	p := map[string]any{"name": name, "server": u.Hostname(), "port": port, "udp": true}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("URI 查询参数无效")
	}
	keys := ""
	switch u.Scheme {
	case "trojan":
		keys = "sni peer security type host path serviceName fp alpn allowInsecure insecure"
	case "vless":
		keys = "sni peer security type host path serviceName fp alpn flow pbk sid allowInsecure insecure encryption"
	case "hysteria2", "hy2":
		keys = "sni peer alpn insecure obfs obfs-password"
	}
	allowed := map[string]bool{}
	for _, key := range strings.Fields(keys) {
		allowed[key] = true
	}
	for k := range q {
		if !allowed[k] || len(q[k]) != 1 {
			return nil, fmt.Errorf("URI 参数 %s 尚不支持，请使用 YAML 订阅", k)
		}
	}
	switch u.Scheme {
	case "ss":
		method, password, ok := strings.Cut(u.User.String(), ":")
		if ok {
			method, _ = url.PathUnescape(method)
			password, _ = url.PathUnescape(password)
		} else {
			b, e := decodeBase64(u.User.Username())
			if e != nil {
				return nil, e
			}
			method, password, ok = strings.Cut(string(b), ":")
		}
		if !ok || method == "" || password == "" {
			return nil, fmt.Errorf("SS 凭据无效")
		}
		p["type"], p["cipher"], p["password"] = "ss", method, password
	case "trojan", "vless":
		if _, hasPassword := u.User.Password(); hasPassword {
			return nil, fmt.Errorf("此 URI 凭据格式不支持，请使用 YAML 订阅")
		}
		security := q.Get("security")
		if security != "" && security != "none" && security != "tls" && !(u.Scheme == "vless" && security == "reality") {
			return nil, fmt.Errorf("URI security 不支持，请使用 YAML 订阅")
		}
		if security == "none" && u.Scheme == "trojan" {
			return nil, fmt.Errorf("Trojan URI 必须使用 TLS")
		}
		if encryption := q.Get("encryption"); encryption != "" && encryption != "none" {
			return nil, fmt.Errorf("此 VLESS encryption 请使用 YAML 订阅")
		}
		p["type"] = u.Scheme
		if u.Scheme == "vless" {
			p["uuid"] = u.User.Username()
			if q.Get("flow") != "" {
				p["flow"] = q.Get("flow")
			}
		} else {
			p["password"] = u.User.Username()
		}
		p["tls"] = u.Scheme == "trojan" || q.Get("security") == "tls" || q.Get("security") == "reality"
		if q.Get("security") == "reality" {
			p["reality-opts"] = map[string]any{"public-key": q.Get("pbk"), "short-id": q.Get("sid")}
		}
		if fp := q.Get("fp"); fp != "" {
			p["client-fingerprint"] = fp
		}
		if typ := q.Get("type"); typ == "ws" {
			p["network"] = "ws"
			p["ws-opts"] = map[string]any{"path": q.Get("path"), "headers": map[string]any{"Host": q.Get("host")}}
		} else if typ == "grpc" {
			p["network"] = "grpc"
			p["grpc-opts"] = map[string]any{"grpc-service-name": q.Get("serviceName")}
		} else if typ != "" && typ != "tcp" {
			return nil, fmt.Errorf("此传输请使用 YAML 订阅")
		}
	case "hysteria2", "hy2":
		p["type"], p["password"] = "hysteria2", u.User.Username()
		if pass, ok := u.User.Password(); ok {
			p["password"] = u.User.Username() + ":" + pass
		}
		for _, k := range []string{"obfs", "obfs-password"} {
			if q.Get(k) != "" {
				p[k] = q.Get(k)
			}
		}
	default:
		return nil, fmt.Errorf("URI 协议 %s 尚不支持，请使用 YAML 订阅", u.Scheme)
	}
	sni := q.Get("sni")
	if sni == "" {
		sni = q.Get("peer")
	}
	if sni != "" {
		key := "sni"
		if u.Scheme == "vless" {
			key = "servername"
		}
		p[key] = sni
	}
	if alpn := q.Get("alpn"); alpn != "" {
		p["alpn"] = strings.Split(alpn, ",")
	}
	for _, key := range []string{"insecure", "allowInsecure"} {
		if value := q.Get(key); value != "" {
			insecure, err := strconv.ParseBool(value)
			if err != nil {
				return nil, fmt.Errorf("URI %s 必须是布尔值", key)
			}
			if insecure {
				p["skip-cert-verify"] = true
			}
		}
	}
	return p, nil
}
