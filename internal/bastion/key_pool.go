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
		m.PublicKey, err = sshkeys.NormalizeList(m.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("使用者 %s 的公钥无效: %w", m.Username, err)
		}
		for _, key := range strings.Split(m.PublicKey, "\n") {
			entry := m
			entry.PublicKey = key
			out = append(out, entry)
		}
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

// ChangeMemberKeys serializes desired-key changes with pool synchronization and
// cleanup. The callback commits the control work queue in the same transaction.
func (h *Handler) ChangeMemberKeys(member, value string, queue func(*sql.Tx) error) error {
	value, err := sshkeys.NormalizeList(value)
	if err != nil {
		return httpapi.NewError(400, err.Error())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.DB.Transaction(func(tx *sql.Tx) error {
		var old, status string
		if err := tx.QueryRow("SELECT ssh_public_key,status FROM members WHERE id=?", member).Scan(&old, &status); err != nil {
			return err
		}
		if status != "active" {
			return httpapi.NewError(409, "使用者正在删除，不能修改公钥")
		}
		wanted := map[string]bool{}
		for _, key := range strings.Split(value, "\n") {
			wanted[key] = true
		}
		old, err = sshkeys.NormalizeList(old)
		if err != nil {
			return err
		}
		for _, key := range strings.Split(old, "\n") {
			if !wanted[key] {
				if _, err := tx.Exec("INSERT OR IGNORE INTO member_key_revocations VALUES(?,?)", member, key); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec("UPDATE members SET ssh_public_key=? WHERE id=?", value, member); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE member_access SET key_state='pending',error='' WHERE member_id=?", member); err != nil {
			return err
		}
		return queue(tx)
	})
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
	wanted := map[string]bool{}
	if key != "" {
		key, err = sshkeys.NormalizeList(key)
		if err != nil {
			return err
		}
		keys := strings.Split(key, "\n")
		for _, k := range keys {
			wanted[k] = true
		}
		if _, err = h.RemoteCommand(ctx, s, commandRequest{Operation: "ensure", Keys: keys}); err != nil {
			return err
		}
	}
	// Keep the revocation journal until every remote operation succeeds. Replay is
	// idempotent if SSH succeeds but the response or local commit is lost.
	return h.DB.Transaction(func(tx *sql.Tx) error {
		revoked, err := platform.Rows(tx, "SELECT public_key FROM member_key_revocations WHERE member_id=?", member)
		if err != nil {
			return err
		}
		if key == "" {
			var current string
			if err = tx.QueryRow("SELECT ssh_public_key FROM members WHERE id=?", member).Scan(&current); err != nil {
				return err
			}
			current, err = sshkeys.NormalizeList(current)
			if err != nil {
				return err
			}
			for _, k := range strings.Split(current, "\n") {
				revoked = append(revoked, map[string]any{"public_key": k})
			}
		}
		members, err := poolMembers(tx, node)
		if err != nil {
			return err
		}
		for _, row := range revoked {
			k := row["public_key"].(string)
			referenced := wanted[k]
			for _, other := range keyReferences(members, k) {
				if other.ID != member {
					referenced = true
				}
			}
			if !referenced {
				if _, err = h.RemoteCommand(ctx, s, commandRequest{Operation: "remove", Key: k}); err != nil {
					return err
				}
			}
		}
		_, err = tx.Exec("DELETE FROM member_key_revocations WHERE member_id=?", member)
		return err
	})
}
func (h *Handler) SyncKeys(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	rows, err := platform.Rows(h.DB.SQL, "SELECT m.id,m.ssh_public_key FROM members m JOIN member_access a ON a.member_id=m.id WHERE m.status='active' AND a.tailscale_id IS NOT NULL")
	if err != nil {
		return err
	}
	problems := []string{}
	for _, row := range rows {
		id := row["id"].(string)
		err = h.editMemberKey(ctx, id, row["ssh_public_key"].(string))
		if err == nil {
			_, err = h.DB.SQL.Exec("UPDATE member_access SET key_state='ready',updated_at=? WHERE member_id=?", platform.Now(), id)
		}
		if err != nil {
			problems = append(problems, id+": "+err.Error())
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
