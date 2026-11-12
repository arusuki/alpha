package containers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"project-alpha/internal/httpapi"
	link "project-alpha/internal/interconnect"
	"project-alpha/internal/platform"
	"strings"
	"time"
)

func runLinkHelper(ctx context.Context, req link.Request) (link.Status, error) {
	var status link.Status
	body, err := json.Marshal(req)
	if err != nil {
		return status, err
	}
	cmd := exec.CommandContext(ctx, "sudo", "-n", "--", link.Helper)
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return status, fmt.Errorf("互联 helper 执行失败，请核对 interconnect init 和服务用户 sudo 权限: %w: %.800s", err, stderr.String())
	}
	if json.Unmarshal(out, &status) != nil || status.Protocol != link.Protocol {
		return status, fmt.Errorf("helper 协议不符，请管理员运行 interconnect init 更新")
	}
	return status, nil
}

type LinkCandidate struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

func (h *Handler) linkCandidates() ([]LinkCandidate, error) {
	cfg, err := h.config()
	if err != nil {
		return nil, err
	}
	records, err := h.records()
	if err != nil {
		return nil, err
	}
	out := []LinkCandidate{}
	for _, r := range records {
		if r.Endpoint == cfg.Endpoint && r.Spec.Network == "bridge" {
			out = append(out, LinkCandidate{r.ID, r.Name, r.Owner})
		}
	}
	return out, nil
}
func (h *Handler) linkVerify(ctx context.Context, endpoints []link.Endpoint) error {
	cfg, err := h.config()
	if err != nil {
		return err
	}
	records, err := h.records()
	if err != nil {
		return err
	}
	byID := map[string]Record{}
	for _, r := range records {
		byID[r.ID] = r
	}
	for _, e := range endpoints {
		r, ok := byID[e.Container]
		if !ok || r.Owner != e.Owner || r.Spec.Network != "bridge" || r.Endpoint != cfg.Endpoint {
			return fmt.Errorf("已接管的 bridge 容器身份或归属发生变化")
		}
		if _, err = h.verify(ctx, r); err != nil {
			return err
		}
	}
	return nil
}
func (h *Handler) linkApply(ctx context.Context, c link.Config, actor string) (link.Status, error) {
	var status link.Status
	if err := c.Validate(); err != nil {
		return status, err
	}
	// This is process state, not a durable pending marker: after an
	// interruption the persisted intention must remain eligible for recovery.
	h.linkApplying.Store(c.Revision)
	defer h.linkApplying.Store(0)
	raw := httpapi.JSONText(c)
	err := h.db.Transaction(func(tx *sql.Tx) error {
		var rev int64
		var previous string
		if err := tx.QueryRow("SELECT desired,config FROM link_local WHERE id=1").Scan(&rev, &previous); err != nil {
			return err
		}
		if rev > c.Revision || rev == c.Revision && raw != previous {
			return fmt.Errorf("配置版本过期或相同版本内容不一致")
		}
		for _, e := range c.Local() {
			if _, err := tx.Exec("INSERT INTO link_claims(container) VALUES(?) ON CONFLICT DO NOTHING", e.Container); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("UPDATE link_local SET desired=?,config=?,error='正在应用' WHERE id=1", c.Revision, raw); err != nil {
			return err
		}
		// Only one apply can run under the handler lock. An older pending row
		// records an interruption between external effects and the final acknowledgement.
		if _, err := tx.Exec("UPDATE link_operations SET result='interrupted' WHERE result='pending'"); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO link_operations(revision,at,result) VALUES(?,?,'pending')", c.Revision, platform.Now()); err != nil {
			return err
		}
		return platform.Audit(tx, actor, "interconnect.apply", fmt.Sprint(c.Revision))
	})
	if err != nil {
		return status, err
	}
	select {
	case h.linkWake <- struct{}{}:
	default:
	}
	req := link.Request{Operation: "guard", Config: c}
	_, err = h.linkHelper(ctx, req)
	if err == nil {
		err = h.linkVerify(ctx, c.Local())
	}
	if err == nil {
		req.Operation = "apply"
		status, err = h.linkHelper(ctx, req)
		if err == nil && status.Applied != c.Revision {
			err = fmt.Errorf("helper 未确认配置版本")
		}
	}
	result := "applied"
	if err != nil {
		result = err.Error()
	}
	storeErr := h.db.Transaction(func(tx *sql.Tx) error {
		if _, e := tx.Exec("UPDATE link_operations SET result=? WHERE id=(SELECT max(id) FROM link_operations)", result); e != nil {
			return e
		}
		if err != nil {
			_, e := tx.Exec("UPDATE link_local SET error=? WHERE id=1", result)
			return e
		}
		if _, e := tx.Exec("DELETE FROM link_claims"); e != nil {
			return e
		}
		for _, endpoint := range c.Local() {
			if _, e := tx.Exec("INSERT INTO link_claims VALUES(?)", endpoint.Container); e != nil {
				return e
			}
		}
		_, e := tx.Exec("UPDATE link_local SET applied=?,error='' WHERE id=1", c.Revision)
		return e
	})
	if storeErr != nil {
		return status, storeErr
	}
	if err == nil && len(c.Local()) == 0 && h.stopLinkEvents != nil {
		h.stopLinkEvents()
	}
	return status, err
}
func (h *Handler) linkDispatch(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	if user.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "容器互联需要管理员权限")
	}
	id, err := h.db.CheckMode("worker")
	if err != nil {
		return 0, nil, err
	}
	if r.Method == "GET" {
		candidates, e := h.linkCandidates()
		if e != nil {
			return 0, nil, e
		}
		var desired, applied int64
		var msg, raw string
		e = h.db.SQL.QueryRow("SELECT desired,applied,error,config FROM link_local").Scan(&desired, &applied, &msg, &raw)
		return 200, map[string]any{"candidates": candidates, "desired": desired, "applied": applied, "error": msg, "applying": h.linkApplying.Load(), "config_digest": fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))}, e
	}
	if r.Method != "POST" {
		return 0, nil, httpapi.NewError(405, "方法无效")
	}
	var req link.Request
	if err = httpapi.DecodeBodyLimit(w, r, &req, 4<<20); err != nil {
		return 0, nil, err
	}
	if req.Config.Node != id || req.Config.Protocol != link.Protocol {
		return 0, nil, httpapi.NewError(409, "worker 身份或互联协议不匹配")
	}
	var result link.Status
	switch req.Operation {
	case "check", "probe":
		result, err = h.linkHelper(r.Context(), req)
	case "apply":
		if err := req.Config.Validate(); err != nil {
			return 0, nil, httpapi.NewError(409, err.Error())
		}
		if h.linkApplying.Load() == req.Config.Revision {
			return 0, nil, httpapi.NewError(503, "worker 正在应用该配置版本，等待执行结果确认")
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if r.Context().Err() != nil {
			return 0, nil, httpapi.NewError(503, "配置请求已中断，等待重新确认执行结果")
		}
		if !req.Force {
			var desired, applied int64
			var saved, failure string
			if err = h.db.SQL.QueryRow("SELECT desired,applied,config,error FROM link_local WHERE id=1").Scan(&desired, &applied, &saved, &failure); err != nil {
				return 0, nil, err
			}
			// Another request can finish between the control's status read
			// and this lock. Only the exact completed payload is idempotent.
			if desired == req.Config.Revision && applied == desired && failure == "" && saved == httpapi.JSONText(req.Config) {
				return 200, link.Status{Protocol: link.Protocol, Applied: applied}, nil
			}
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
		defer cancel()
		result, err = h.linkApply(ctx, req.Config, user.Username)
	default:
		return 0, nil, httpapi.NewError(400, "未知互联操作")
	}
	if err != nil {
		return 0, nil, httpapi.NewError(409, err.Error())
	}
	return 200, result, nil
}

func (h *Handler) linkStored() (link.Config, error) {
	var c link.Config
	var raw string
	err := h.db.SQL.QueryRow("SELECT config FROM link_local WHERE id=1").Scan(&raw)
	if err == nil && raw != "" {
		err = json.Unmarshal([]byte(raw), &c)
	}
	return c, err
}
func (h *Handler) restoreLinks(ctx context.Context, id, network, action string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, err := h.linkStored()
	if err != nil || c.Revision == 0 {
		return
	}
	changed := ""
	if id != "" || network != "" {
		for _, ep := range c.Local() {
			if id == ep.Container || network == ep.Network() {
				changed = ep.Container
				break
			}
		}
		if changed == "" {
			return
		}
	}
	if changed != "" || action == "activate" {
		var applied int64
		var failure string
		if h.db.SQL.QueryRow("SELECT applied,error FROM link_local").Scan(&applied, &failure) == nil && applied == c.Revision && failure == "" {
			if status, e := h.linkHelper(ctx, link.Request{Operation: "check", Config: c}); e == nil && status.Applied == c.Revision {
				return
			}
			if changed != "" && h.linkVerify(ctx, c.Local()) == nil {
				status, e := h.linkHelper(ctx, link.Request{Operation: "refresh", Config: c, Container: changed})
				if e == nil && status.Applied == c.Revision {
					return
				}
			}
		}
	}
	if _, err = h.linkApply(ctx, c, "worker-recovery"); err != nil && ctx.Err() == nil {
		log.Printf("互联恢复 %s: %v", action, err)
	}
}

// Subscribe only while the node has reserved endpoints. Retry subscriptions on
// disconnect and replay the persisted version before consuming missed events.
func (h *Handler) WatchLinks(ctx context.Context) {
	reason := "startup"
	for ctx.Err() == nil {
		// Register cancellation under the same lock as the last endpoint's
		// withdrawal, including withdrawal racing subscription startup.
		watchCtx, stop := context.WithCancel(ctx)
		h.mu.Lock()
		var pending bool
		err := h.db.SQL.QueryRow("SELECT desired<>applied OR EXISTS(SELECT 1 FROM link_claims) FROM link_local").Scan(&pending)
		if pending {
			h.stopLinkEvents = stop
		}
		h.mu.Unlock()
		if err != nil {
			stop()
			return
		}
		if !pending {
			stop()
			select {
			case <-ctx.Done():
				return
			case <-h.linkWake:
				reason = "activate"
				continue
			}
		}
		since := fmt.Sprint(time.Now().Unix())
		h.restoreLinks(ctx, "", "", reason)
		reason = "reconnect"
		cfg, err := h.config()
		if err != nil {
			stop()
			return
		}
		cmd := exec.CommandContext(watchCtx, "docker", "--host", cfg.Endpoint, "events", "--since", since, "--format", "{{json .}}", "--filter", "type=container", "--filter", "type=network", "--filter", "event=start", "--filter", "event=die", "--filter", "event=destroy", "--filter", "event=connect", "--filter", "event=disconnect")
		pipe, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err == nil {
			scan := bufio.NewScanner(pipe)
			for scan.Scan() {
				var ev struct {
					Type, Action string
					Actor        struct {
						ID         string
						Attributes map[string]string
					}
				}
				if json.Unmarshal(scan.Bytes(), &ev) != nil {
					continue
				}
				id, network := ev.Actor.ID, ""
				if ev.Type == "network" {
					id = ev.Actor.Attributes["container"]
					network = ev.Actor.Attributes["name"]
				}
				if strings.TrimSpace(id+network) != "" {
					h.restoreLinks(ctx, id, network, ev.Action)
				}
			}
			_ = cmd.Wait()
		}
		stop()
		h.mu.Lock()
		h.stopLinkEvents = nil
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}
