// Package members owns machine users, independently of platform login accounts.
package members

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"project-alpha/internal/credentials"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
	"project-alpha/internal/sshkeys"
)

//go:embed schema.sql
var schemaSQL string

func Initialize(tx *sql.Tx) error {
	_, err := tx.Exec(schemaSQL)
	return err
}

type Store struct{ *platform.Database }
type Field struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Type     string   `json:"type"`
	Required bool     `json:"required"`
	Options  []string `json:"options,omitempty"`
}
type Schema struct {
	Revision int     `json:"revision"`
	Fields   []Field `json:"fields"`
}
type Member struct {
	SSHKey        string            `json:"ssh_public_key"`
	ResourceToken string            `json:"-"`
	Status        string            `json:"status"`
	ID            string            `json:"id"`
	Username      string            `json:"username"`
	Profile       map[string]string `json:"profile"`
	Schema        Schema            `json:"schema"`
	InvitationID  string            `json:"invitation_id"`
	CreatedAt     float64           `json:"created_at"`
}
type Invitation struct {
	ID        string  `json:"id"`
	Label     string  `json:"label"`
	Quota     int     `json:"quota"`
	Used      int     `json:"used"`
	Remaining int     `json:"remaining"`
	Status    string  `json:"status"`
	CreatedBy string  `json:"created_by"`
	CreatedAt float64 `json:"created_at"`
	Code      string  `json:"code,omitempty"`
}
type queryRower interface{ QueryRow(string, ...any) *sql.Row }

func readSchema(q queryRower) (Schema, error) {
	var s Schema
	var raw string
	err := q.QueryRow("SELECT revision,fields FROM member_registration_schema WHERE id=1").Scan(&s.Revision, &raw)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal([]byte(raw), &s.Fields)
	return s, err
}
func (s *Store) Schema() (Schema, error) { return readSchema(s.SQL) }

var fieldKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
var username = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,31}$`)
var codePattern = regexp.MustCompile(`^[a-f0-9]{48}$`)

func (s *Store) SaveSchema(next Schema, actor string) (Schema, error) {
	if next.Revision < 1 || next.Fields == nil || len(next.Fields) > 32 {
		return next, httpapi.NewError(400, "schema 版本无效或字段超过 32 项；fields 必须是数组")
	}
	seen := map[string]bool{}
	for i := range next.Fields {
		f := &next.Fields[i]
		f.Label = strings.TrimSpace(f.Label)
		if !fieldKey.MatchString(f.Key) || f.Key == "constructor" || f.Key == "prototype" || seen[f.Key] || f.Label == "" || utf8.RuneCountInString(f.Label) > 80 {
			return next, httpapi.NewError(400, "字段标识需唯一，为小写字母开头的 1–48 位字母、数字或下划线；名称需为 1–80 个字符")
		}
		seen[f.Key] = true
		if f.Type != "text" && f.Type != "select" {
			return next, httpapi.NewError(400, "字段类型仅支持 text 或 select")
		}
		if f.Type == "text" && len(f.Options) > 0 || f.Type == "select" && (len(f.Options) < 1 || len(f.Options) > 64) {
			return next, httpapi.NewError(400, "文本字段不接受选项；单选字段需配置 1–64 个选项")
		}
		options := map[string]bool{}
		for j, option := range f.Options {
			option = strings.TrimSpace(option)
			if option == "" || utf8.RuneCountInString(option) > 80 || options[option] || strings.ContainsAny(option, "\r\n\x00") {
				return next, httpapi.NewError(400, "选项需为不重复的 1–80 个字符，且不能包含换行")
			}
			options[option], f.Options[j] = true, option
		}
	}
	err := s.Transaction(func(tx *sql.Tx) error {
		result, err := tx.Exec("UPDATE member_registration_schema SET revision=revision+1,fields=? WHERE id=1 AND revision=?", httpapi.JSONText(next.Fields), next.Revision)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return httpapi.NewError(409, "注册字段已被更新，请重新载入后修改")
		}
		return platform.Audit(tx, actor, "member.schema", fmt.Sprintf("%d fields", len(next.Fields)))
	})
	if err == nil {
		next.Revision++
	}
	return next, err
}

func hashCode(code string) string { h := sha256.Sum256([]byte(code)); return hex.EncodeToString(h[:]) }

const invitationKeyFile = "invitation-code.key"

func invitationCodeError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return httpapi.NewError(500, "邀请码加密密钥 "+invitationKeyFile+" 丢失；请恢复原密钥文件或使用新数据目录")
	}
	return httpapi.NewError(500, "邀请码加密密钥或密文不可用；请检查或恢复数据目录中的 "+invitationKeyFile+"（权限须为 0600）及密文，或使用新数据目录")
}

// InvitationCode is used only when an administrator requests a registry link.
// Listing invitations never decrypts or exposes their codes.
func (s *Store) InvitationCode(id string) (string, error) {
	var ciphertext, hash string
	err := s.SQL.QueryRow("SELECT code_ciphertext,code_hash FROM member_invitations WHERE id=? AND revoked=0 AND used<quota", id).Scan(&ciphertext, &hash)
	if err == sql.ErrNoRows {
		return "", httpapi.NewError(400, "邀请码不存在、已用尽或已作废，请重新选择")
	}
	if err != nil {
		return "", err
	}
	code, err := credentials.Decrypt(s.Directory, invitationKeyFile, "member.invitation/"+id, ciphertext)
	if err != nil {
		return "", invitationCodeError(err)
	}
	if !codePattern.MatchString(code) {
		return "", httpapi.NewError(500, "邀请码密文内容无效；请恢复原数据或使用新数据目录")
	}
	if hashCode(code) != hash {
		return "", httpapi.NewError(500, "邀请码摘要与密文不匹配；请恢复原数据或使用新数据目录")
	}
	return code, nil
}

// CheckInvitation never consumes a slot; RegisterWith rechecks it transactionally.
func (s *Store) CheckInvitation(code string) error {
	if !codePattern.MatchString(code) {
		return httpapi.NewError(400, "邀请码无效或已失效")
	}
	var id string
	err := s.SQL.QueryRow("SELECT id FROM member_invitations WHERE code_hash=? AND revoked=0 AND used<quota", hashCode(code)).Scan(&id)
	if err == sql.ErrNoRows {
		return httpapi.NewError(400, "邀请码无效或已失效")
	}
	return err
}
func (s *Store) CreateInvitation(label string, quota int, actor string) (Invitation, error) {
	label = strings.TrimSpace(label)
	i := Invitation{ID: platform.RandomHex(16), Label: label, Quota: quota, Remaining: quota, Status: "active", CreatedBy: actor, CreatedAt: platform.Now(), Code: platform.RandomHex(24)}
	if quota < 1 || quota > 100000 || utf8.RuneCountInString(label) > 100 {
		return Invitation{}, httpapi.NewError(400, "quota 必须为 1–100000 的整数，备注最多 100 个字符")
	}
	err := s.Transaction(func(tx *sql.Tx) error {
		var existing bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM member_invitations)").Scan(&existing); err != nil {
			return err
		}
		encrypt := credentials.Encrypt
		if existing {
			encrypt = credentials.EncryptExisting
		}
		ciphertext, err := encrypt(s.Directory, invitationKeyFile, "member.invitation/"+i.ID, i.Code)
		if err != nil {
			return invitationCodeError(err)
		}
		if _, err := tx.Exec("INSERT INTO member_invitations(id,code_hash,code_ciphertext,label,quota,created_by,created_at) VALUES(?,?,?,?,?,?,?)", i.ID, hashCode(i.Code), ciphertext, label, quota, actor, i.CreatedAt); err != nil {
			return err
		}
		return platform.Audit(tx, actor, "member.invitation.create", fmt.Sprintf("%s / quota=%d", i.ID, quota))
	})
	return i, err
}
func (s *Store) Invitations() ([]Invitation, error) {
	rows, err := s.SQL.Query("SELECT id,label,quota,used,revoked,created_by,created_at FROM member_invitations ORDER BY created_at DESC,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Invitation{}
	for rows.Next() {
		var i Invitation
		var revoked bool
		if err := rows.Scan(&i.ID, &i.Label, &i.Quota, &i.Used, &revoked, &i.CreatedBy, &i.CreatedAt); err != nil {
			return nil, err
		}
		i.Remaining, i.Status = i.Quota-i.Used, "active"
		if i.Remaining == 0 {
			i.Status = "exhausted"
		}
		if revoked {
			i.Status, i.Remaining = "revoked", 0
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
func (s *Store) UpdateInvitationLabel(id, label, actor string) error {
	label = strings.TrimSpace(label)
	if utf8.RuneCountInString(label) > 100 {
		return httpapi.NewError(400, "备注最多 100 个字符")
	}
	return s.Transaction(func(tx *sql.Tx) error {
		result, err := tx.Exec("UPDATE member_invitations SET label=? WHERE id=?", label, id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return httpapi.NewError(404, "邀请码不存在")
		}
		return platform.Audit(tx, actor, "member.invitation.update", id)
	})
}

func (s *Store) RevokeInvitation(id, actor string) error {
	return s.Transaction(func(tx *sql.Tx) error {
		var revoked bool
		if err := tx.QueryRow("SELECT revoked FROM member_invitations WHERE id=?", id).Scan(&revoked); err != nil {
			if err == sql.ErrNoRows {
				return httpapi.NewError(404, "邀请码不存在")
			}
			return err
		}
		if revoked {
			return nil
		}
		if _, err := tx.Exec("UPDATE member_invitations SET revoked=1 WHERE id=?", id); err != nil {
			return err
		}
		return platform.Audit(tx, actor, "member.invitation.revoke", id)
	})
}

func (s *Store) DeleteInvitation(id, actor string) error {
	return s.Transaction(func(tx *sql.Tx) error {
		result, err := tx.Exec("DELETE FROM member_invitations WHERE id=?", id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return httpapi.NewError(404, "邀请码不存在")
		}
		return platform.Audit(tx, actor, "member.invitation.delete", id)
	})
}

type Registration struct {
	Password       string                     `json:"password"`
	SSHKey         string                     `json:"ssh_public_key"`
	Username       string                     `json:"username"`
	InvitationCode string                     `json:"invitation_code"`
	SchemaRevision int                        `json:"schema_revision"`
	Profile        map[string]json.RawMessage `json:"profile"`
}

func validateProfile(fields []Field, values map[string]json.RawMessage) (map[string]string, error) {
	known := map[string]bool{}
	out := map[string]string{}
	for _, f := range fields {
		known[f.Key] = true
		var value string
		if raw, ok := values[f.Key]; ok {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
				return nil, httpapi.NewError(400, f.Label+"必须是字符串")
			}
		}
		value = strings.TrimSpace(value)
		if value == "" {
			if f.Required {
				return nil, httpapi.NewError(400, "请填写"+f.Label)
			}
			continue
		}
		if utf8.RuneCountInString(value) > 512 || strings.ContainsRune(value, '\x00') {
			return nil, httpapi.NewError(400, f.Label+"最多 512 个字符，且不能包含 NUL")
		}
		if f.Type == "select" {
			found := false
			for _, option := range f.Options {
				if value == option {
					found = true
					break
				}
			}
			if !found {
				return nil, httpapi.NewError(400, f.Label+"的选项无效")
			}
		}
		out[f.Key] = value
	}
	for key := range values {
		if !known[key] {
			return nil, httpapi.NewError(400, "未定义的用户信息字段："+key)
		}
	}
	return out, nil
}
func (s *Store) RegisterWith(req Registration, reserve func(*sql.Tx, Member) error) (Member, error) {
	return s.registerWithToken(req, reserve, "")
}

// A registry persists this secret before sending a registration. Retrying after
// a lost reply recovers the same member without consuming another invitation slot.
func (s *Store) registerWithToken(req Registration, reserve func(*sql.Tx, Member) error, token string) (Member, error) {
	m := Member{ID: platform.RandomHex(16), Username: req.Username, CreatedAt: platform.Now()}
	if !username.MatchString(req.Username) || req.Username == "data" {
		return m, httpapi.NewError(400, "使用者标识需为小写字母开头的 3–32 位字母、数字、下划线或短横线，且不能为 data")
	}
	if !codePattern.MatchString(req.InvitationCode) {
		return m, httpapi.NewError(400, "邀请码无效或已失效")
	}
	if req.SchemaRevision < 1 || req.Profile == nil {
		return m, httpapi.NewError(400, "请提供 schema_revision 和 profile 对象")
	}
	key, keyErr := sshkeys.Normalize(req.SSHKey)
	if keyErr != nil {
		return m, httpapi.NewError(400, keyErr.Error())
	}
	if err := ValidatePassword(req.Password); err != nil {
		return m, err
	}
	passwordHash, err := platform.PasswordHash(req.Password, "")
	if err != nil {
		return m, err
	}
	m.SSHKey = key
	m.ResourceToken = platform.RandomHex(32)
	if token != "" {
		m.ResourceToken = token
	}
	m.Status = "active"
	err = s.Transaction(func(tx *sql.Tx) error {
		if token != "" {
			var previous Member
			var profile, schema, invitationHash, previousPassword string
			err := tx.QueryRow(`SELECT id,username,profile,registration_schema,ssh_public_key,status,invitation_id,created_at,invitation_code_hash,password_hash FROM members WHERE resource_token_hash=?`, hashCode(token)).Scan(&previous.ID, &previous.Username, &profile, &schema, &previous.SSHKey, &previous.Status, &previous.InvitationID, &previous.CreatedAt, &invitationHash, &previousPassword)
			if err == nil {
				if err = json.Unmarshal([]byte(schema), &previous.Schema); err != nil {
					return err
				}
				if err = json.Unmarshal([]byte(profile), &previous.Profile); err != nil {
					return err
				}
				values, e := validateProfile(previous.Schema.Fields, req.Profile)
				if e != nil || !platform.CheckPassword(req.Password, previousPassword) || previous.Status != "active" || previous.Username != req.Username || previous.SSHKey != key || previous.Schema.Revision != req.SchemaRevision || invitationHash != hashCode(req.InvitationCode) || httpapi.JSONText(values) != httpapi.JSONText(previous.Profile) {
					return httpapi.NewError(409, "此注册请求已提交，不能更改注册内容")
				}
				previous.ResourceToken = token
				m = previous
				return nil
			}
			if err != sql.ErrNoRows {
				return err
			}
		}
		var err error
		m.Schema, err = readSchema(tx)
		if err != nil {
			return err
		}
		if req.SchemaRevision != m.Schema.Revision {
			return httpapi.NewError(409, "注册字段已更新，请重新获取 schema 后提交")
		}
		m.Profile, err = validateProfile(m.Schema.Fields, req.Profile)
		if err != nil {
			return err
		}
		if err = tx.QueryRow("SELECT id FROM member_invitations WHERE code_hash=? AND revoked=0 AND used<quota", hashCode(req.InvitationCode)).Scan(&m.InvitationID); err != nil {
			if err == sql.ErrNoRows {
				return httpapi.NewError(400, "邀请码无效或已失效")
			}
			return err
		}
		result, err := tx.Exec("UPDATE member_invitations SET used=used+1 WHERE id=? AND revoked=0 AND used<quota", m.InvitationID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return httpapi.NewError(400, "邀请码无效或已失效")
		}
		ciphertext, err := s.encryptPassword(tx, m.ID, req.Password)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO members(id,username,profile,registration_schema,invitation_id,invitation_code_hash,created_at,ssh_public_key,resource_token_hash,status,password_hash,password_ciphertext) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", m.ID, m.Username, httpapi.JSONText(m.Profile), httpapi.JSONText(m.Schema), m.InvitationID, hashCode(req.InvitationCode), m.CreatedAt, m.SSHKey, hashCode(m.ResourceToken), m.Status, passwordHash, ciphertext); err != nil {
			if platform.IsConstraint(err) {
				return httpapi.NewError(409, "该使用者标识已注册")
			}
			return err
		}
		if reserve != nil {
			if err = reserve(tx, m); err != nil {
				return err
			}
		}
		return platform.Audit(tx, "registration", "member.register", m.Username+" / invitation="+m.InvitationID)
	})
	return m, err
}
func (s *Store) Members() ([]Member, error) {
	rows, err := s.SQL.Query("SELECT id,username,profile,registration_schema,invitation_id,created_at,ssh_public_key,status FROM members ORDER BY created_at DESC,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		var profile, schema string
		if err = rows.Scan(&m.ID, &m.Username, &profile, &schema, &m.InvitationID, &m.CreatedAt, &m.SSHKey, &m.Status); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(profile), &m.Profile); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(schema), &m.Schema); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
