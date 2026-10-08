package mihomo

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

// Control owns subscription refresh and durable configuration delivery. Its
// timer only retries pending work / due subscriptions; it never polls health.
type Control struct {
	Store       Store
	Local       *Manager
	Targets     func() ([]Target, error)
	Remote      func(context.Context, Target, string, string, any, any) error
	FetchClient *http.Client
	cancel      context.CancelFunc
	done        chan struct{}
	wake        chan struct{}
	mu          sync.Mutex
	force       map[string]bool
}

func NewControl(db *platform.Database, local *Manager, targets func() ([]Target, error), remote func(context.Context, Target, string, string, any, any) error) *Control {
	c := &Control{Store: Store{db}, Local: local, Targets: targets, Remote: remote, wake: make(chan struct{}, 1), done: make(chan struct{}), force: map[string]bool{}}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	go func() {
		defer close(c.done)
		timer := time.NewTicker(30 * time.Second)
		defer timer.Stop()
		for {
			c.reconcile(ctx)
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
			case <-timer.C:
			}
		}
	}()
	return c
}
func (c *Control) Close() { c.cancel(); <-c.done }
func (c *Control) Wake(target string, force bool) {
	c.mu.Lock()
	if force {
		c.force[target] = true
	}
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}
func sourceVersion(s Settings, t Target) string {
	version := fmt.Sprintf("%s:%d", t.ID, s.Revision)
	if s.Inherited {
		version = fmt.Sprintf("control:%d/inherit:%d", s.BaseRevision, s.Revision)
	}
	return fmt.Sprintf("%s/%s/%s", version, t.Role, t.Name)
}
func configured(s Settings) bool { return s.BaseRevision > 0 || !s.Inherited && s.Revision > 0 }

func (c *Control) reconcile(ctx context.Context) {
	targets, err := c.Targets()
	if err != nil {
		log.Printf("mihomo targets: %v", err)
		return
	}
	c.mu.Lock()
	force := c.force
	c.force = map[string]bool{}
	c.mu.Unlock()
	type fetched struct {
		proxies []Proxy
		err     error
	}
	cache := map[string]fetched{}
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		s, err := c.Store.Settings(target.ID)
		if err != nil {
			log.Printf("mihomo settings: %v", err)
			continue
		}
		if !configured(s) {
			continue
		}
		v, err := c.Store.Sync(target.ID)
		if err != nil {
			log.Printf("mihomo delivery state: %v", err)
			continue
		}
		version := sourceVersion(s, target)
		due := s.Config.RefreshMinutes > 0 && platform.Now()-v.RefreshedAt >= float64(s.Config.RefreshMinutes*60)
		refresh := force["all"] || force[target.ID] || v.SourceVersion != version || due
		if !refresh && v.Delivered {
			continue
		}
		v.AttemptedAt = platform.Now()
		if refresh {
			key := fmt.Sprintf("%s:%d", target.ID, s.Revision)
			if s.Inherited || target.ID == "control" {
				key = fmt.Sprintf("control:%d", s.BaseRevision)
			}
			f, ok := cache[key]
			if !ok {
				fetchCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
				f.proxies, f.err = Fetch(fetchCtx, s.Config, c.FetchClient)
				cancel()
				cache[key] = f
			}
			var bundle Bundle
			err = f.err
			if err == nil {
				bundle, err = Render(s.Config, f.proxies, target)
			}
			if err != nil {
				v.Error = err.Error()
				if e := c.Store.saveSync(target.ID, v); e != nil {
					log.Printf("mihomo persist error: %v", e)
				}
				continue
			}
			v.SourceVersion = version
			v.RefreshedAt = platform.Now()
			v.Error = ""
			v.Delivered = v.Delivered && v.Bundle.Digest == bundle.Digest && !force["all"] && !force[target.ID]
			v.Bundle = bundle
			if err = c.Store.saveSync(target.ID, v); err != nil {
				log.Printf("mihomo persist bundle: %v", err)
				continue
			}
		}
		latest, err := c.Store.Settings(target.ID)
		if err != nil || sourceVersion(latest, target) != version {
			continue
		}
		if !v.Delivered {
			callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			if target.ID == "control" {
				err = c.Local.Apply(callCtx, v.Bundle)
			} else {
				err = c.Remote(callCtx, target, "PUT", Path+"/apply", v.Bundle, new(json.RawMessage))
			}
			cancel()
			v.Delivered = err == nil
			v.Error = ""
			if err != nil {
				v.Error = err.Error()
			}
			if err = c.Store.saveSync(target.ID, v); err != nil {
				log.Printf("mihomo persist delivery: %v", err)
			}
		}
	}
}

type NodeStatus struct {
	Target
	Inherited bool      `json:"inherited"`
	Online    bool      `json:"online"`
	Pending   bool      `json:"pending"`
	Sync      SyncState `json:"sync"`
	Service   Status    `json:"service"`
	Error     string    `json:"error"`
}

func (c *Control) statuses(ctx context.Context, targets []Target) []NodeStatus {
	out := make([]NodeStatus, len(targets))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, t := range targets {
		wg.Go(func() {
			v := NodeStatus{Target: t, Service: Status{Groups: []Group{}}}
			defer func() { out[i] = v }()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				v.Error = "读取节点状态已取消"
				return
			}
			s, err := c.Store.Settings(t.ID)
			if err != nil {
				v.Error = err.Error()
				return
			}
			v.Inherited = s.Inherited
			v.Sync, err = c.Store.Sync(t.ID)
			if err != nil {
				v.Error = err.Error()
				return
			}
			v.Pending = configured(s) && (!v.Sync.Delivered || v.Sync.SourceVersion != sourceVersion(s, t))
			callCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			if t.ID == "control" {
				v.Service = c.Local.Status(callCtx)
			} else {
				err = c.Remote(callCtx, t, "GET", Path+"/status", nil, &v.Service)
			}
			v.Online = err == nil
			if err != nil {
				v.Error = err.Error()
				return
			}
			if v.Sync.Bundle.Digest != "" && v.Service.Digest != v.Sync.Bundle.Digest {
				v.Pending = true
			}
		})
	}
	wg.Wait()
	return out
}

func (c *Control) Dispatch(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if user.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "代理配置需要管理员权限")
	}
	targets, err := c.Targets()
	if err != nil {
		return 0, nil, err
	}
	if r.URL.Path == "/api/mihomo/status" && r.Method == "GET" {
		return 200, map[string]any{"nodes": c.statuses(r.Context(), targets)}, nil
	}
	if r.URL.Path == "/api/mihomo/refresh" && r.Method == "POST" {
		c.Wake("all", true)
		return 202, map[string]bool{"queued": true}, nil
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/mihomo/"), "/")
	if len(parts) != 2 {
		return 0, nil, httpapi.NewError(404, "代理接口不存在")
	}
	var target Target
	for _, t := range targets {
		if t.ID == parts[0] {
			target = t
			break
		}
	}
	if target.ID == "" {
		return 0, nil, httpapi.NewError(404, "节点不存在")
	}
	switch parts[1] {
	case "settings":
		if r.Method == "PUT" {
			var input SaveSettings
			if err = decode(w, r, &input); err != nil {
				return 0, nil, err
			}
			if err = c.Store.Save(target.ID, input, user.Username); err != nil {
				return 0, nil, err
			}
			c.Wake(target.ID, false)
		} else if r.Method != "GET" {
			break
		}
		v, err := c.Store.Settings(target.ID)
		return 200, v, err
	case "preview":
		if r.Method != "POST" {
			break
		}
		var config Config
		if err = decode(w, r, &config); err != nil {
			return 0, nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		proxies, err := Fetch(ctx, config, c.FetchClient)
		if err != nil {
			return 0, nil, httpapi.NewError(400, err.Error())
		}
		b, err := Render(config, proxies, target)
		if err != nil {
			return 0, nil, httpapi.NewError(400, err.Error())
		}
		return 200, b, nil
	case "refresh":
		if r.Method != "POST" {
			break
		}
		c.Wake(target.ID, true)
		return 202, map[string]bool{"queued": true}, nil
	case "service", "selection":
		if (parts[1] == "service" && r.Method != "POST") || (parts[1] == "selection" && r.Method != "PUT") {
			break
		}
		clone := r.Clone(r.Context())
		clone.URL.Path = Path + "/" + parts[1]
		if target.ID == "control" {
			return c.Local.Dispatch(w, clone)
		}
		var body json.RawMessage
		if err = decode(w, r, &body); err != nil {
			return 0, nil, err
		}
		var result json.RawMessage
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		if err = c.Remote(ctx, target, r.Method, clone.URL.Path, body, &result); err != nil {
			return 0, nil, err
		}
		if err = platform.Audit(c.Store.DB.SQL, user.Username, "mihomo."+parts[1], target.ID); err != nil {
			return 0, nil, err
		}
		return 200, result, nil
	}
	return 0, nil, httpapi.NewError(405, "代理接口不支持此方法")
}
