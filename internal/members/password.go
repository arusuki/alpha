package members

import (
	"database/sql"
	"net/http"
	"strings"

	"project-alpha/internal/credentials"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

const passwordKeyFile = "member-password.key"

func passwordStorageError() error {
	return httpapi.NewError(500, "使用者密码密钥或密文不可用；请恢复数据目录中的 member-password.key（权限须为 0600）及原密文，或使用新数据目录")
}

func ValidatePassword(password string) error {
	if len(password) < 12 || len(password) > 256 || strings.ContainsAny(password, "\x00\r\n:") {
		return httpapi.NewError(400, "密码需为 12–256 字节，不能含换行、冒号或 NUL")
	}
	return nil
}

func (s *Store) encryptPassword(tx *sql.Tx, id, password string) (string, error) {
	var existing bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM members)").Scan(&existing); err != nil {
		return "", err
	}
	encrypt := credentials.Encrypt
	if existing {
		encrypt = credentials.EncryptExisting
	}
	ciphertext, err := encrypt(s.Directory, passwordKeyFile, "member.password/"+id, password)
	if err != nil {
		return "", passwordStorageError()
	}
	return ciphertext, nil
}

// InitialPassword is used only for provisioning, never in member responses.
func (s *Store) InitialPassword(id string) (string, error) {
	var ciphertext string
	if err := s.SQL.QueryRow("SELECT password_ciphertext FROM members WHERE id=? AND status='active'", id).Scan(&ciphertext); err != nil {
		return "", err
	}
	password, err := credentials.Decrypt(s.Directory, passwordKeyFile, "member.password/"+id, ciphertext)
	if err != nil {
		return "", passwordStorageError()
	}
	return password, ValidatePassword(password)
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request, username string) (string, error) {
	if err := h.checkAttempts(r); err != nil {
		return "", err
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := httpapi.DecodeBody(w, r, &req); err != nil {
		return "", err
	}
	if err := ValidatePassword(req.Password); err != nil {
		return "", httpapi.NewError(401, "用户名或密码错误")
	}
	return h.store.LoginStatus(username, req.Password)
}

func (s *Store) LoginStatus(username, password string) (string, error) {
	var id, hash string
	err := s.SQL.QueryRow("SELECT id,password_hash FROM members WHERE username=? AND status='active'", username).Scan(&id, &hash)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	// Perform the same password derivation for unknown usernames.
	if err == sql.ErrNoRows {
		hash = strings.Repeat("0", 32) + ":" + strings.Repeat("0", 64)
	}
	if !platform.CheckPassword(password, hash) || id == "" {
		return "", httpapi.NewError(401, "用户名或密码错误")
	}
	token := platform.RandomHex(32)
	err = s.Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM member_sessions WHERE expires_at<=?", platform.Now()); err != nil {
			return err
		}
		result, err := tx.Exec("INSERT INTO member_sessions(token_hash,member_id,expires_at) SELECT ?,id,? FROM members WHERE id=? AND password_hash=? AND status='active'", hashCode(token), platform.Now()+12*3600, id, hash)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return httpapi.NewError(401, "密码已修改，请重新登录")
		}
		return nil
	})
	return token, err
}

func (s *Store) SessionID(token string) (string, error) {
	if len(token) != 64 {
		return "", httpapi.NewError(401, "请使用注册时设置的密码登录")
	}
	var id string
	err := s.SQL.QueryRow("SELECT m.id FROM member_sessions s JOIN members m ON m.id=s.member_id WHERE s.token_hash=? AND s.expires_at>? AND m.status='active'", hashCode(token), platform.Now()).Scan(&id)
	if err == sql.ErrNoRows {
		return "", httpapi.NewError(401, "登录已失效，请重新输入密码")
	}
	return id, err
}

func (s *Store) Logout(token string) error {
	_, err := s.SQL.Exec("DELETE FROM member_sessions WHERE token_hash=?", hashCode(token))
	return err
}

func (s *Store) ChangePassword(id, old, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	if err := ValidatePassword(old); err != nil {
		return httpapi.NewError(403, "当前密码错误")
	}
	hash, err := platform.PasswordHash(password, "")
	if err != nil {
		return err
	}
	return s.Transaction(func(tx *sql.Tx) error {
		var previous string
		if err := tx.QueryRow("SELECT password_hash FROM members WHERE id=? AND status='active'", id).Scan(&previous); err != nil {
			return err
		}
		if !platform.CheckPassword(old, previous) {
			return httpapi.NewError(403, "当前密码错误")
		}
		ciphertext, err := s.encryptPassword(tx, id, password)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE members SET password_hash=?,password_ciphertext=? WHERE id=?", hash, ciphertext, id); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM member_sessions WHERE member_id=?", id); err != nil {
			return err
		}
		return platform.Audit(tx, id, "member.password.change", id)
	})
}
