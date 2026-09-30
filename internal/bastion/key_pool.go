package bastion

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
	"project-alpha/internal/sshkeys"
)

type poolMember struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Status    string `json:"status"`
	PublicKey string `json:"-"`
}
type poolKey struct {
	ID          string       `json:"id"`
	NodeID      string       `json:"node_id"`
	NodeName    string       `json:"node_name"`
	PublicKey   string       `json:"public_key"`
	Fingerprint string       `json:"fingerprint"`
	State       string       `json:"state"`
	Members     []poolMember `json:"members"`
}

func poolMembers(q platform.Queryer, node string) ([]poolMember, error) {
	rows, err := q.Query(`SELECT m.id,m.username,m.ssh_public_key,m.status FROM members m JOIN member_access a ON a.member_id=m.id WHERE a.tailscale_id=? AND (m.status='active' OR a.key_state<>'deleted') ORDER BY m.username,m.id`, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []poolMember{}
	for rows.Next() {
		var m poolMember
		if err = rows.Scan(&m.ID, &m.Username, &m.PublicKey, &m.Status); err != nil {
			return nil, err
		}
		m.PublicKey, err = sshkeys.Normalize(m.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("使用者 %s 的公钥无效: %w", m.Username, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func keyReferences(members []poolMember, key string) []poolMember {
	out := []poolMember{}
	for _, m := range members {
		if m.PublicKey == key {
			out = append(out, m)
		}
	}
	return out
}
func indexPool(s ShareNode, v keySnapshot, members []poolMember) []poolKey {
	index := map[string][]poolMember{}
	for _, member := range members {
		index[member.PublicKey] = append(index[member.PublicKey], member)
	}
	out := make([]poolKey, 0, len(v.Keys))
	for id, key := range v.Keys {
		blob, _ := base64.StdEncoding.DecodeString(strings.Fields(key)[1])
		digest := sha256.Sum256(blob)
		refs := index[key]
		if refs == nil {
			refs = []poolMember{}
		}
		state := "free"
		if len(refs) > 0 {
			state = "used"
		}
		out = append(out, poolKey{id, s.ID, s.Name, key, "SHA256:" + base64.RawStdEncoding.EncodeToString(digest[:]), state, refs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func ensurePoolKeys(v *keySnapshot, keys ...string) {
	present := map[string]bool{}
	for _, key := range v.Keys {
		present[key] = true
	}
	for _, key := range keys {
		if present[key] {
			continue
		}
		for {
			id := platform.RandomHex(16)
			if _, ok := v.Keys[id]; !ok {
				v.Keys[id] = key
				present[key] = true
				break
			}
		}
	}
}
func (h *Handler) keyPool(ctx context.Context) ([]poolKey, string, error) {
	shares, err := h.shares()
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 13*time.Second)
	defer cancel()
	keys := make([][]poolKey, len(shares))
	problems := make([]string, len(shares))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, s := range shares {
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				problems[i] = s.Name + ": SSH 查询超时"
				return
			}
			reply, e := h.RemoteCommand(ctx, s, commandRequest{Operation: "inspect"})
			if e != nil {
				problems[i] = s.Name + ": " + e.Error()
				return
			}
			members, e := poolMembers(h.DB.SQL, s.ID)
			if e != nil {
				problems[i] = s.Name + ": " + e.Error()
				return
			}
			keys[i] = indexPool(s, keySnapshot{Version: reply.Version, Keys: reply.Keys}, members)
		})
	}
	wg.Wait()
	out := []poolKey{}
	errors := []string{}
	for i := range shares {
		out = append(out, keys[i]...)
		if problems[i] != "" {
			errors = append(errors, problems[i])
		}
	}
	return out, strings.Join(errors, "；"), nil
}
func (h *Handler) editMemberKey(ctx context.Context, member, key string) error {
	var node string
	if err := h.DB.SQL.QueryRow("SELECT COALESCE(tailscale_id,'') FROM member_access WHERE member_id=?", member).Scan(&node); err != nil {
		return err
	}
	if node == "" {
		if key == "" {
			return nil
		}
		return fmt.Errorf("尚未分配 share node，无法发布跳板公钥")
	}
	s, err := h.share(node)
	if err != nil {
		return err
	}
	if key != "" {
		key, err = sshkeys.Normalize(key)
		if err != nil {
			return err
		}
		_, err = h.RemoteCommand(ctx, s, commandRequest{Operation: "ensure", Keys: []string{key}})
		return err
	}
	return h.DB.Transaction(func(tx *sql.Tx) error {
		var revoked string
		if err = tx.QueryRow("SELECT ssh_public_key FROM members WHERE id=?", member).Scan(&revoked); err != nil {
			return err
		}
		revoked, err = sshkeys.Normalize(revoked)
		if err != nil {
			return err
		}
		members, err := poolMembers(tx, node)
		if err != nil {
			return err
		}
		for _, other := range keyReferences(members, revoked) {
			if other.ID != member {
				return nil
			}
		}
		_, err = h.RemoteCommand(ctx, s, commandRequest{Operation: "remove", Key: revoked})
		return err
	})
}
func (h *Handler) SyncKeys(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	shares, err := h.shares()
	if err != nil {
		return err
	}
	problems := []string{}
	for _, s := range shares {
		err = h.DB.Transaction(func(tx *sql.Tx) error {
			members, e := poolMembers(tx, s.ID)
			if e != nil {
				return e
			}
			keys := []string{}
			for _, m := range members {
				if m.Status == "active" {
					keys = append(keys, m.PublicKey)
				}
			}
			if len(keys) == 0 {
				return nil
			}
			if _, e = h.RemoteCommand(ctx, s, commandRequest{Operation: "ensure", Keys: keys}); e != nil {
				return e
			}
			_, e = tx.Exec("UPDATE member_access SET key_state='ready',updated_at=? WHERE tailscale_id=? AND member_id IN (SELECT id FROM members WHERE status='active')", platform.Now(), s.ID)
			return e
		})
		if err != nil {
			problems = append(problems, s.Name+": "+err.Error())
		}
	}
	if len(problems) > 0 {
		return httpapi.NewError(502, strings.Join(problems, "；"))
	}
	return nil
}
func (h *Handler) cleanFreeKey(ctx context.Context, node, entry, actor string) error {
	if !sshkeys.ID.MatchString(entry) {
		return httpapi.NewError(400, "公钥池条目标识无效")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s, err := h.share(node)
	if err == sql.ErrNoRows {
		return httpapi.NewError(404, "分享节点不存在")
	}
	if err != nil {
		return err
	}
	return h.DB.Transaction(func(tx *sql.Tx) error {
		reply, e := h.RemoteCommand(ctx, s, commandRequest{Operation: "inspect"})
		if e != nil {
			return e
		}
		key, ok := reply.Keys[entry]
		if !ok {
			return httpapi.NewError(404, "公钥条目已不存在，请刷新")
		}
		members, e := poolMembers(tx, node)
		if e != nil {
			return e
		}
		if len(keyReferences(members, key)) > 0 {
			return httpapi.NewError(409, "该公钥已有使用者关联，不能按 free 清理")
		}
		if _, e = h.RemoteCommand(ctx, s, commandRequest{Operation: "clean", Entry: entry}); e != nil {
			return e
		}
		return platform.Audit(tx, actor, "bastion.key.clean", node+"/"+entry)
	})
}
