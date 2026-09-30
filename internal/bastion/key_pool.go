package bastion

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strings"

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
	PublicKey   string       `json:"public_key"`
	Fingerprint string       `json:"fingerprint"`
	State       string       `json:"state"`
	Members     []poolMember `json:"members"`
}

// Membership is indexed by normalized public-key content, independently of
// the pool's entry IDs or the control that originally published those entries.
func poolMembers(q platform.Queryer) ([]poolMember, error) {
	rows, err := q.Query(`SELECT m.id,m.username,m.ssh_public_key,m.status FROM members m
 LEFT JOIN member_access a ON a.member_id=m.id
 WHERE m.status='active' OR COALESCE(a.key_state,'pending')<>'deleted' ORDER BY m.username,m.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []poolMember{}
	for rows.Next() {
		var m poolMember
		if err = rows.Scan(&m.ID, &m.Username, &m.PublicKey, &m.Status); err != nil {
			return nil, err
		}
		m.PublicKey, err = sshkeys.Normalize(m.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("使用者 %s 的公钥无效: %w", m.Username, err)
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

func keyReferences(members []poolMember, key string) []poolMember {
	refs := []poolMember{}
	for _, member := range members {
		if member.PublicKey == key {
			refs = append(refs, member)
		}
	}
	return refs
}

func indexPool(v keySnapshot, members []poolMember) []poolKey {
	index := map[string][]poolMember{}
	for _, member := range members {
		index[member.PublicKey] = append(index[member.PublicKey], member)
	}
	result := make([]poolKey, 0, len(v.Keys))
	for id, key := range v.Keys {
		blob, _ := base64.StdEncoding.DecodeString(strings.Fields(key)[1])
		digest := sha256.Sum256(blob)
		refs := index[key]
		if refs == nil {
			refs = []poolMember{}
		}
		entry := poolKey{ID: id, PublicKey: key, Fingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(digest[:]), State: "free", Members: refs}
		if len(entry.Members) > 0 {
			entry.State = "used"
		}
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func ensurePoolKey(v *keySnapshot, key string) {
	ensurePoolKeys(v, []string{key})
}

func ensurePoolKeys(v *keySnapshot, keys []string) {
	present := map[string]bool{}
	for _, existing := range v.Keys {
		present[existing] = true
	}
	for _, key := range keys {
		if present[key] {
			continue
		}
		for {
			id := platform.RandomHex(16)
			if _, exists := v.Keys[id]; !exists {
				v.Keys[id] = key
				present[key] = true
				break
			}
		}
	}
}

func (s keyStore) snapshot(control string) (keySnapshot, error) {
	r, c, err := s.open()
	if err != nil {
		return keySnapshot{}, err
	}
	defer r.Close()
	if c.ControlID != control || c.ServiceUID != os.Geteuid() {
		return keySnapshot{}, fmt.Errorf("请先接管已有账号，再查看和管理公钥池")
	}
	k, err := openKeys(r, c)
	if err != nil {
		return keySnapshot{}, err
	}
	defer k.Close()
	return loadSnapshot(k, c)
}

func (h *Handler) keyPool() ([]poolKey, error) {
	id, err := h.DB.CheckMode("control")
	if err != nil {
		return nil, err
	}
	v, err := h.keys.snapshot(id)
	if err != nil {
		return nil, err
	}
	members, err := poolMembers(h.DB.SQL)
	if err != nil {
		return nil, err
	}
	return indexPool(v, members), nil
}

func (h *Handler) editMemberKey(member, key string) error {
	id, err := h.DB.CheckMode("control")
	if err != nil {
		return err
	}
	return h.DB.Transaction(func(tx *sql.Tx) error {
		if key != "" {
			key, err = sshkeys.Normalize(key)
			if err != nil {
				return err
			}
			return h.keys.update(id, func(v *keySnapshot) error { ensurePoolKey(v, key); return nil })
		}
		var revoked string
		if err = tx.QueryRow("SELECT ssh_public_key FROM members WHERE id=?", member).Scan(&revoked); err != nil {
			return err
		}
		revoked, err = sshkeys.Normalize(revoked)
		if err != nil {
			return err
		}
		members, err := poolMembers(tx)
		if err != nil {
			return err
		}
		return h.keys.update(id, func(v *keySnapshot) error {
			for _, other := range keyReferences(members, revoked) {
				if other.ID != member {
					return nil
				}
			}
			for entry, existing := range v.Keys {
				if existing == revoked {
					delete(v.Keys, entry)
				}
			}
			return nil
		})
	})
}

// SyncKeys adds missing current members' keys without removing free entries.
// It is also called by the installer as the ordinary service user.
func (h *Handler) SyncKeys() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	id, err := h.DB.CheckMode("control")
	if err != nil {
		return err
	}
	return h.DB.Transaction(func(tx *sql.Tx) error {
		members, err := poolMembers(tx)
		if err != nil {
			return err
		}
		if err = h.keys.update(id, func(v *keySnapshot) error {
			keys := []string{}
			for _, m := range members {
				if m.Status == "active" {
					keys = append(keys, m.PublicKey)
				}
			}
			ensurePoolKeys(v, keys)
			return nil
		}); err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE member_access SET key_state='ready',updated_at=? WHERE member_id IN (SELECT id FROM members WHERE status='active')", platform.Now())
		return err
	})
}

func (h *Handler) cleanFreeKey(entry, actor string) error {
	if !sshkeys.ID.MatchString(entry) {
		return httpapi.NewError(400, "公钥池条目标识无效")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	id, err := h.DB.CheckMode("control")
	if err != nil {
		return err
	}
	// Registration and member updates serialize with this transaction. A key
	// that acquired a user since the page loaded must not be cleaned as free.
	return h.DB.Transaction(func(tx *sql.Tx) error {
		members, err := poolMembers(tx)
		if err != nil {
			return err
		}
		if err = h.keys.update(id, func(v *keySnapshot) error {
			key, exists := v.Keys[entry]
			if !exists {
				return httpapi.NewError(404, "公钥条目已不存在，请刷新")
			}
			if len(keyReferences(members, key)) > 0 {
				return httpapi.NewError(409, "该公钥已有使用者关联，不能按 free 清理")
			}
			delete(v.Keys, entry)
			return nil
		}); err != nil {
			return err
		}
		return platform.Audit(tx, actor, "bastion.key.clean", entry)
	})
}
