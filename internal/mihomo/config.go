// Package mihomo manages subscription rendering and a per-service proxy process.
package mihomo

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const MaxConfig = 2 << 20
const MaxSubscription = 8 << 20
const MaxProxies = 10000
const Path = "/api/node-mihomo"

type Subscription struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Content string `json:"content"`
	Prefix  string `json:"prefix"`
}

// Named filters operate on names, protocol types and subscription names.
// Include and Exclude are RE2 regular expressions, not country guesses.
type Filter struct {
	Name    string   `json:"name"`
	Include string   `json:"include"`
	Exclude string   `json:"exclude"`
	Types   []string `json:"types"`
	Sources []string `json:"sources"`
}

type Config struct {
	Binary         string         `json:"binary"`
	MixedPort      int            `json:"mixed_port"`
	RefreshMinutes int            `json:"refresh_minutes"`
	Subscriptions  []Subscription `json:"subscriptions"`
	Filters        []Filter       `json:"filters"`
	Filter         string         `json:"filter"`
	Template       string         `json:"template"`
}

const DefaultTemplate = `mixed-port: {{ .MixedPort }}
allow-lan: false
mode: rule
log-level: warning
proxies:
{{ yaml .Proxies | indent 2 }}
proxy-groups:
  - name: PROXY
    type: select
    proxies:
      - DIRECT
{{ range .Names }}      - {{ quote . }}
{{ end }}rules:
  - MATCH,PROXY
`

func DefaultConfig() Config {
	return Config{Binary: "mihomo", MixedPort: 7890, RefreshMinutes: 720,
		Subscriptions: []Subscription{}, Filters: []Filter{}, Template: DefaultTemplate}
}

var labelPattern = regexp.MustCompile(`^[\p{L}\p{N}_. -]{1,80}$`)

func (c Config) Validate() error {
	if c.Binary != "mihomo" && (!filepath.IsAbs(c.Binary) || strings.ContainsAny(c.Binary, "\x00\r\n")) {
		return fmt.Errorf("mihomo 路径需为绝对路径，或使用 PATH 中的 mihomo")
	}
	if c.MixedPort < 1024 || c.MixedPort > 65535 {
		return fmt.Errorf("代理端口需为 1024–65535")
	}
	if c.RefreshMinutes != 0 && (c.RefreshMinutes < 5 || c.RefreshMinutes > 10080) {
		return fmt.Errorf("订阅刷新间隔需为 5–10080 分钟；0 表示仅手动刷新")
	}
	if len(c.Subscriptions) > 32 || len(c.Filters) > 64 || len(c.Template) > 128<<10 || strings.TrimSpace(c.Template) == "" {
		return fmt.Errorf("最多 32 个订阅、64 个过滤规则；模板不能为空或超过 128 KiB")
	}
	sources := map[string]bool{}
	for _, s := range c.Subscriptions {
		if !labelPattern.MatchString(s.Name) || strings.TrimSpace(s.Name) != s.Name || strings.TrimSpace(s.Name) == "" || sources[s.Name] {
			return fmt.Errorf("订阅名称无效或重复：%q", s.Name)
		}
		sources[s.Name] = true
		if (s.URL == "") == (strings.TrimSpace(s.Content) == "") {
			return fmt.Errorf("订阅 %s 必须填写 URL 或内联内容其中之一", s.Name)
		}
		if len(s.Content) > MaxSubscription || len(s.Prefix) > 80 || strings.ContainsAny(s.Prefix, "\x00\r\n") {
			return fmt.Errorf("订阅 %s 内容或名称前缀超出限制", s.Name)
		}
		if s.URL != "" {
			u, err := url.Parse(s.URL)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
				return fmt.Errorf("订阅 %s URL 必须为不含用户信息或 fragment 的 HTTP(S) 地址", s.Name)
			}
		}
	}
	filters := map[string]bool{"all": true}
	for _, f := range c.Filters {
		if !labelPattern.MatchString(f.Name) || strings.TrimSpace(f.Name) != f.Name || strings.TrimSpace(f.Name) == "" || filters[f.Name] {
			return fmt.Errorf("过滤规则名称无效或重复：%q（all 为保留名称）", f.Name)
		}
		filters[f.Name] = true
		for _, pattern := range []string{f.Include, f.Exclude} {
			if len(pattern) > 2048 {
				return fmt.Errorf("过滤规则 %s 表达式过长", f.Name)
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("过滤规则 %s 正则表达式无效：%w", f.Name, err)
			}
		}
		for _, source := range f.Sources {
			if !sources[source] {
				return fmt.Errorf("过滤规则 %s 引用了不存在的订阅 %s", f.Name, source)
			}
		}
	}
	if c.Filter != "" && !filters[c.Filter] {
		return fmt.Errorf("全局过滤规则不存在：%s", c.Filter)
	}
	_, err := parseTemplate(c.Template, nil)
	return err
}

type Proxy struct {
	Value  map[string]any
	Source string
}

func (c Config) filter(name string, proxies []Proxy) ([]Proxy, error) {
	if name == "" || name == "all" {
		return proxies, nil
	}
	for _, f := range c.Filters {
		if f.Name != name {
			continue
		}
		include, _ := regexp.Compile(f.Include)
		exclude, _ := regexp.Compile(f.Exclude)
		out := []Proxy{}
		for _, p := range proxies {
			n, _ := p.Value["name"].(string)
			t, _ := p.Value["type"].(string)
			if f.Include != "" && !include.MatchString(n) || f.Exclude != "" && exclude.MatchString(n) {
				continue
			}
			if len(f.Types) > 0 && !slices.Contains(f.Types, t) || len(f.Sources) > 0 && !slices.Contains(f.Sources, p.Source) {
				continue
			}
			out = append(out, p)
		}
		return out, nil
	}
	return nil, fmt.Errorf("过滤规则不存在：%s", name)
}
