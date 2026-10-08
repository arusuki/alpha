package mihomo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

type Target struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}
type Group struct {
	Name     string   `json:"name" yaml:"name"`
	Type     string   `json:"type" yaml:"type"`
	Proxies  []string `json:"proxies" yaml:"proxies"`
	Now      string   `json:"now,omitempty" yaml:"-"`
	Resolved string   `json:"resolved,omitempty" yaml:"-"`
}
type Bundle struct {
	Binary     string  `json:"binary"`
	YAML       string  `json:"yaml"`
	Digest     string  `json:"digest"`
	Groups     []Group `json:"groups"`
	ProxyCount int     `json:"proxy_count"`
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > MaxConfig {
		return 0, fmt.Errorf("生成配置超过 2 MiB")
	}
	return b.Buffer.Write(p)
}

func parseTemplate(source string, names func(string) ([]string, error)) (*template.Template, error) {
	if names == nil {
		names = func(string) ([]string, error) { return nil, nil }
	}
	return template.New("mihomo").Option("missingkey=error").Funcs(template.FuncMap{
		"names": names,
		"quote": func(v any) (string, error) { b, e := json.Marshal(v); return string(b), e },
		"yaml":  func(v any) (string, error) { b, e := yaml.Marshal(v); return strings.TrimSuffix(string(b), "\n"), e },
		"indent": func(n int, s string) (string, error) {
			if n < 0 || n > 32 {
				return "", fmt.Errorf("indent 需为 0–32")
			}
			p := strings.Repeat(" ", n)
			return p + strings.ReplaceAll(s, "\n", "\n"+p), nil
		},
	}).Parse(source)
}

func Render(c Config, proxies []Proxy, node Target) (Bundle, error) {
	if err := c.Validate(); err != nil {
		return Bundle{}, err
	}
	values := []map[string]any{}
	all := []string{}
	for _, p := range proxies {
		values = append(values, p.Value)
		all = append(all, p.Value["name"].(string))
	}
	t, err := parseTemplate(c.Template, func(name string) ([]string, error) {
		list, err := c.filter(name, proxies)
		if err != nil {
			return nil, err
		}
		out := []string{}
		for _, p := range list {
			out = append(out, p.Value["name"].(string))
		}
		return out, nil
	})
	if err != nil {
		return Bundle{}, fmt.Errorf("模板语法错误：%w", err)
	}
	var out limitedBuffer
	if err = t.Execute(&out, struct {
		Node      Target
		MixedPort int
		Proxies   []map[string]any
		Names     []string
	}{node, c.MixedPort, values, all}); err != nil {
		return Bundle{}, fmt.Errorf("模板生成失败：%w", err)
	}
	config, groups, count, err := inspectConfig(out.Bytes())
	if err != nil {
		return Bundle{}, err
	}
	// Management endpoints are always private to the node. Never inherit a
	// subscription's public dashboard, DoH listener or external-controller.
	for k := range config {
		if strings.HasPrefix(k, "external-") || k == "secret" {
			delete(config, k)
		}
	}
	profile, ok := config["profile"].(map[string]any)
	if !ok {
		profile = map[string]any{}
	}
	profile["store-selected"] = false
	config["profile"] = profile
	b, err := yaml.Marshal(config)
	if err != nil {
		return Bundle{}, err
	}
	v := Bundle{Binary: c.Binary, YAML: string(b), Groups: groups, ProxyCount: count}
	v.Digest = bundleDigest(v)
	return v, nil
}

func bundleDigest(b Bundle) string {
	sum := sha256.Sum256([]byte(b.Binary + "\x00" + b.YAML))
	return hex.EncodeToString(sum[:])
}

func inspectConfig(raw []byte) (map[string]any, []Group, int, error) {
	if len(raw) > MaxConfig {
		return nil, nil, 0, fmt.Errorf("生成配置超过 2 MiB")
	}
	var config map[string]any
	if err := decodeYAML(raw, &config); err != nil {
		return nil, nil, 0, err
	}
	if len(config) == 0 {
		return nil, nil, 0, fmt.Errorf("模板必须生成 YAML 配置对象")
	}
	// Central subscription filters need a complete, reproducible candidate set.
	if v, ok := config["proxy-providers"]; ok && v != nil {
		return nil, nil, 0, fmt.Errorf("请在订阅列表中管理代理来源，不在模板内定义 proxy-providers")
	}
	proxies, ok := config["proxies"].([]any)
	if !ok {
		return nil, nil, 0, fmt.Errorf("模板必须生成 proxies 列表（允许空列表）")
	}
	known := map[string]bool{}
	for k := range builtins {
		known[k] = true
	}
	for _, p := range proxies {
		v, ok := p.(map[string]any)
		if !ok {
			return nil, nil, 0, fmt.Errorf("proxies 中存在无效节点")
		}
		n, _ := v["name"].(string)
		typ, _ := v["type"].(string)
		if !validName(n) || typ == "" || known[n] {
			return nil, nil, 0, fmt.Errorf("代理名称或类型无效、重名：%q", n)
		}
		known[n] = true
	}
	rawGroups, ok := config["proxy-groups"].([]any)
	if !ok || len(rawGroups) == 0 {
		return nil, nil, 0, fmt.Errorf("模板必须包含非空 proxy-groups")
	}
	groups := []Group{}
	for _, item := range rawGroups {
		v, ok := item.(map[string]any)
		if !ok {
			return nil, nil, 0, fmt.Errorf("策略组格式无效")
		}
		n, _ := v["name"].(string)
		typ, _ := v["type"].(string)
		if !validName(n) || known[n] {
			return nil, nil, 0, fmt.Errorf("策略组名称无效或重名：%q", n)
		}
		switch typ {
		case "select", "url-test", "fallback", "load-balance", "relay":
		default:
			return nil, nil, 0, fmt.Errorf("策略组 %s 类型无效", n)
		}
		if v["use"] != nil || v["include-all"] == true || v["include-all-proxies"] == true || v["include-all-providers"] == true {
			return nil, nil, 0, fmt.Errorf("策略组 %s 请用 names 生成明确的候选列表", n)
		}
		members, ok := v["proxies"].([]any)
		if !ok || len(members) == 0 {
			return nil, nil, 0, fmt.Errorf("策略组 %s 没有候选节点；请检查过滤规则", n)
		}
		g := Group{Name: n, Type: typ, Proxies: []string{}}
		for _, m := range members {
			s, ok := m.(string)
			if !ok {
				return nil, nil, 0, fmt.Errorf("策略组 %s 候选名称必须是字符串", n)
			}
			g.Proxies = append(g.Proxies, s)
		}
		groups = append(groups, g)
		known[n] = true
	}
	byName := map[string]Group{}
	for _, g := range groups {
		byName[g.Name] = g
		for _, n := range g.Proxies {
			if !known[n] {
				return nil, nil, 0, fmt.Errorf("策略组 %s 引用了不存在的节点 %s", g.Name, n)
			}
		}
	}
	visited := map[string]int{}
	var visit func(string) error
	visit = func(n string) error {
		if visited[n] == 1 {
			return fmt.Errorf("策略组存在循环引用：%s", n)
		}
		if visited[n] == 2 {
			return nil
		}
		visited[n] = 1
		for _, m := range byName[n].Proxies {
			if _, ok := byName[m]; ok {
				if err := visit(m); err != nil {
					return err
				}
			}
		}
		visited[n] = 2
		return nil
	}
	for _, g := range groups {
		if err := visit(g.Name); err != nil {
			return nil, nil, 0, err
		}
	}
	return config, groups, len(proxies), nil
}
