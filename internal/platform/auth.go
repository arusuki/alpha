package platform

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"project-alpha/internal/httpapi"
)

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func tokenHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// PasswordHash derives a PBKDF2-SHA256 hash; an empty salt generates a fresh salt.
func PasswordHash(password, salt string) (string, error) {
	if salt == "" {
		salt = RandomHex(16)
	}
	bytes, err := hex.DecodeString(salt)
	if err != nil {
		return "", err
	}
	hash, err := pbkdf2.Key(sha256.New, password, bytes, 300000, 32)
	if err != nil {
		return "", err
	}
	return salt + ":" + hex.EncodeToString(hash), nil
}

// CheckPassword verifies a password against a stored PBKDF2-SHA256 hash.
func CheckPassword(password, encoded string) bool {
	parts := strings.Split(encoded, ":")
	if len(parts) != 2 {
		return false
	}
	actual, err := PasswordHash(password, parts[0])
	return err == nil && hmac.Equal([]byte(actual), []byte(encoded))
}
func (d *Database) Configured() (bool, error) {
	var count int
	err := d.SQL.QueryRow("SELECT count(*) FROM users").Scan(&count)
	return count > 0, err
}
func (d *Database) Users() ([]object, error) {
	return Rows(d.SQL, "SELECT id,username,role,enabled,created_at FROM users ORDER BY created_at")
}
func (d *Database) CreateUser(value map[string]json.RawMessage, actor string, setup bool) (object, error) {
	for key := range value {
		if key != "username" && key != "password" && key != "role" {
			return nil, httpapi.NewError(400, "账号字段无效")
		}
	}
	name, password := httpapi.FieldString(value, "username"), httpapi.FieldString(value, "password")
	role := "viewer"
	if setup {
		role = "admin"
	}
	if _, ok := value["role"]; ok {
		role = httpapi.FieldString(value, "role")
	}
	if !usernamePattern.MatchString(name) {
		return nil, httpapi.NewError(400, "账号名需为 3–48 位字母、数字、点、下划线或短横线")
	}
	if n := utf8.RuneCountInString(password); n < 12 || n > 256 {
		return nil, httpapi.NewError(400, "密码长度需为 12–256 个字符")
	}
	if (role != "admin" && role != "viewer") || (setup && role != "admin") {
		return nil, httpapi.NewError(400, "角色无效")
	}
	encoded, err := PasswordHash(password, "")
	if err != nil {
		return nil, err
	}
	uid := RandomHex(16)
	err = d.Transaction(func(tx *sql.Tx) error {
		if setup {
			var n int
			if err := tx.QueryRow("SELECT count(*) FROM users").Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return httpapi.NewError(409, "管理员已初始化，请登录")
			}
		}
		if _, err := tx.Exec("INSERT INTO users VALUES(?,?,?,?,?,?)", uid, name, encoded, role, 1, Now()); err != nil {
			if IsConstraint(err) {
				return httpapi.NewError(409, "该账号名已存在")
			}
			return err
		}
		return Audit(tx, actor, "user.create", name+" / "+role)
	})
	if err != nil {
		return nil, err
	}
	return object{"id": uid, "username": name, "role": role, "enabled": 1}, nil
}
func (d *Database) UpdateUser(uid string, value map[string]json.RawMessage, actor string) error {
	var enabled bool
	role := httpapi.FieldString(value, "role")
	if len(value) != 2 || string(value["enabled"]) == "null" || json.Unmarshal(value["enabled"], &enabled) != nil || (role != "admin" && role != "viewer") {
		return httpapi.NewError(400, "账号状态或角色无效")
	}
	return d.Transaction(func(tx *sql.Tx) error {
		var name string
		if err := tx.QueryRow("SELECT username FROM users WHERE id=?", uid).Scan(&name); err != nil {
			if err == sql.ErrNoRows {
				return httpapi.NewError(404, "账号不存在")
			}
			return err
		}
		if name == actor && (!enabled || role != "admin") {
			return httpapi.NewError(409, "不能禁用或降级当前管理员")
		}
		var n int
		if err := tx.QueryRow("SELECT count(*) FROM users WHERE enabled=1 AND role='admin' AND id<>?", uid).Scan(&n); err != nil {
			return err
		}
		if n == 0 && (!enabled || role != "admin") {
			return httpapi.NewError(409, "必须保留至少一名启用的管理员")
		}
		if _, err := tx.Exec("UPDATE users SET enabled=?,role=? WHERE id=?", enabled, role, uid); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM sessions WHERE user_id=?", uid); err != nil {
			return err
		}
		return Audit(tx, actor, "user.update", name)
	})
}

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}
type Session struct {
	User User   `json:"user"`
	CSRF string `json:"csrf"`
}

func (d *Database) Login(name, password string) (string, *Session, error) {
	if utf8.RuneCountInString(name) > 48 || utf8.RuneCountInString(password) > 256 {
		return "", nil, httpapi.NewError(400, "账号或密码格式无效")
	}
	var u User
	var encoded string
	err := d.SQL.QueryRow("SELECT id,username,role,password_hash FROM users WHERE username=? AND enabled=1", name).Scan(&u.ID, &u.Username, &u.Role, &encoded)
	if err != nil && err != sql.ErrNoRows {
		return "", nil, err
	}
	if err == sql.ErrNoRows {
		encoded = strings.Repeat("0", 32) + ":" + strings.Repeat("0", 64)
	}
	if !CheckPassword(password, encoded) || u.ID == "" {
		return "", nil, httpapi.NewError(401, "账号或密码错误")
	}
	token, csrf := randomToken(), randomToken()
	err = d.Transaction(func(tx *sql.Tx) error {
		// Recheck after hashing: disabled users or changed credentials cannot mint a stale session.
		var count int
		if err := tx.QueryRow("SELECT count(*) FROM users WHERE id=? AND enabled=1 AND password_hash=? AND role=?", u.ID, encoded, u.Role).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return httpapi.NewError(401, "账号或密码错误")
		}
		if _, err := tx.Exec("DELETE FROM sessions WHERE expires_at<?", Now()); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO sessions VALUES(?,?,?,?)", tokenHash(token), u.ID, csrf, Now()+43200); err != nil {
			return err
		}
		return Audit(tx, name, "session.login", "")
	})
	return token, &Session{u, csrf}, err
}
func (d *Database) Session(token string) (*Session, error) {
	s := &Session{}
	err := d.SQL.QueryRow("SELECT u.id,u.username,u.role,s.csrf FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires_at>? AND u.enabled=1", tokenHash(token), Now()).Scan(&s.User.ID, &s.User.Username, &s.User.Role, &s.CSRF)
	if err == sql.ErrNoRows {
		return nil, httpapi.NewError(401, "请登录管理平台")
	}
	return s, err
}
func (d *Database) Logout(token string) error {
	_, err := d.SQL.Exec("DELETE FROM sessions WHERE token_hash=?", tokenHash(token))
	return err
}
func (d *Database) ChangePassword(uid, old, new string) error {
	if utf8.RuneCountInString(old) > 256 || utf8.RuneCountInString(new) < 12 || utf8.RuneCountInString(new) > 256 {
		return httpapi.NewError(400, "新密码需为 12–256 个字符")
	}
	encoded, err := PasswordHash(new, "")
	if err != nil {
		return err
	}
	return d.Transaction(func(tx *sql.Tx) error {
		var hash, name string
		err := tx.QueryRow("SELECT password_hash,username FROM users WHERE id=? AND enabled=1", uid).Scan(&hash, &name)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == sql.ErrNoRows || !CheckPassword(old, hash) {
			return httpapi.NewError(400, "原密码不正确")
		}
		if _, err = tx.Exec("UPDATE users SET password_hash=? WHERE id=?", encoded, uid); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM sessions WHERE user_id=?", uid); err != nil {
			return err
		}
		return Audit(tx, name, "user.password", "")
	})
}

func (d *Database) AuditRows() ([]object, error) {
	return Rows(d.SQL, "SELECT * FROM audit ORDER BY id DESC LIMIT 100")
}

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,48}$`)
