package bastion

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
	"project-alpha/internal/sshkeys"
)

type sshSettings struct {
	Revision     int64  `json:"revision"`
	IdentityFile string `json:"identity_file"`
}

type sshIdentity struct {
	IdentityFile string `json:"identity_file"`
	PublicKey    string `json:"public_key"`
	Fingerprint  string `json:"fingerprint"`
}

func (h *Handler) sshSettings() (sshSettings, error) {
	var cfg sshSettings
	err := h.DB.SQL.QueryRow("SELECT revision,identity_file FROM bastion_ssh_settings WHERE id=1").Scan(&cfg.Revision, &cfg.IdentityFile)
	return cfg, err
}

func identityPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	if len(path) > 4096 || !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return "", httpapi.NewError(400, "请选择总控已创建的密钥")
	}
	return filepath.Clean(path), nil
}

// Do not change permissions or replace identities supplied by the service user.
func checkPrivateIdentity(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return httpapi.NewError(400, "总控服务用户无法读取私钥文件，请检查路径和权限")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 65536 || (info.Mode().Perm() != 0600 && info.Mode().Perm() != 0400) || !ok || owner.Uid != uint32(os.Geteuid()) {
		return httpapi.NewError(400, "私钥需为总控服务用户持有的普通文件，权限为 0600 或 0400")
	}
	return nil
}

func inspectIdentity(ctx context.Context, path string) (sshIdentity, error) {
	identity := sshIdentity{IdentityFile: path}
	if err := checkPrivateIdentity(path); err != nil {
		return identity, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Derive the public key from the private key; an adjacent .pub may be stale.
	cmd := exec.CommandContext(ctx, "ssh-keygen", "-y", "-P", "", "-f", path)
	output := &limitedBuffer{limit: 16384}
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		return identity, httpapi.NewError(400, "无法读取 SSH 私钥；请选择有效的无口令私钥。总控需安装 ssh-keygen")
	}
	key, err := sshkeys.Normalize(string(output.data))
	if err != nil {
		return identity, err
	}
	blob, _ := base64.StdEncoding.DecodeString(strings.Fields(key)[1])
	digest := sha256.Sum256(blob)
	identity.PublicKey = key
	identity.Fingerprint = "SHA256:" + base64.RawStdEncoding.EncodeToString(digest[:])
	return identity, nil
}

func (h *Handler) identityDirectory() string {
	return filepath.Join(h.DB.Directory, "control-ssh")
}

// Only identities in the managed key directory can be selected or used.
func (h *Handler) checkManagedIdentity(path string) error {
	root := h.identityDirectory()
	directory := filepath.Dir(path)
	if path == "" || filepath.Base(path) != "id_ed25519" || filepath.Dir(directory) != root || filepath.Clean(path) != path {
		return httpapi.NewError(400, "请选择总控已创建的密钥；没有密钥时请先创建")
	}
	for _, name := range []string{root, directory, path} {
		info, err := os.Lstat(name)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return httpapi.NewError(400, "总控密钥不存在或为符号链接，请重新选择或创建密钥")
		}
		if name != path {
			owner, ok := info.Sys().(*syscall.Stat_t)
			if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || owner.Uid != uint32(os.Geteuid()) {
				return httpapi.NewError(400, "密钥目录需为总控服务用户持有的 0700 目录")
			}
		}
	}
	return checkPrivateIdentity(path)
}

func (h *Handler) inspectManagedIdentity(ctx context.Context, path string) (sshIdentity, error) {
	if err := h.checkManagedIdentity(path); err != nil {
		return sshIdentity{}, err
	}
	return inspectIdentity(ctx, path)
}

func (h *Handler) availableIdentities() []string {
	result := []string{}
	entries, _ := os.ReadDir(h.identityDirectory())
	for _, entry := range entries {
		path := filepath.Join(h.identityDirectory(), entry.Name(), "id_ed25519")
		if entry.IsDir() && h.checkManagedIdentity(path) == nil {
			result = append(result, path)
		}
	}
	sort.Strings(result)
	return result
}

func (h *Handler) publicSSHSettings(ctx context.Context) (any, error) {
	cfg, err := h.sshSettings()
	if err != nil {
		return nil, err
	}
	result := struct {
		sshSettings
		Identities   []string `json:"identities"`
		KeyDirectory string   `json:"key_directory"`
		PublicKey    string   `json:"public_key"`
		Fingerprint  string   `json:"fingerprint"`
		Error        string   `json:"error"`
	}{sshSettings: cfg, Identities: h.availableIdentities(), KeyDirectory: h.identityDirectory()}
	if cfg.IdentityFile != "" {
		identity, e := h.inspectManagedIdentity(ctx, cfg.IdentityFile)
		if e != nil {
			result.Error = e.Error()
		} else {
			result.PublicKey, result.Fingerprint = identity.PublicKey, identity.Fingerprint
		}
	}
	return result, nil
}

func (h *Handler) saveSSHSettings(ctx context.Context, cfg sshSettings, actor string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	old, err := h.sshSettings()
	if err != nil {
		return err
	}
	if cfg.Revision != old.Revision {
		return httpapi.NewError(409, "主控 SSH 配置已被修改，请重新载入")
	}
	cfg.IdentityFile, err = identityPath(cfg.IdentityFile)
	if err != nil {
		return err
	}
	if _, err = h.inspectManagedIdentity(ctx, cfg.IdentityFile); err != nil {
		return err
	}
	if cfg.IdentityFile != old.IdentityFile {
		ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		defer cancel()
		nodes, err := h.shares()
		if err != nil {
			return err
		}
		// Disabled nodes may still have members, so check every configured node.
		for _, node := range nodes {
			if _, err = h.sshCommandWithIdentity(ctx, node, commandRequest{Operation: "inspect"}, cfg.IdentityFile); err != nil {
				return httpapi.NewError(409, fmt.Sprintf("SSH 配置未保存：分享节点 %s 校验失败。请先在所有 share node 授权候选公钥并核对主机信任：%v", node.Name, err))
			}
		}
	}
	err = h.DB.Transaction(func(tx *sql.Tx) error {
		result, err := tx.Exec("UPDATE bastion_ssh_settings SET identity_file=?,revision=revision+1 WHERE id=1 AND revision=?", cfg.IdentityFile, cfg.Revision)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return httpapi.NewError(409, "主控 SSH 配置已被修改，请重新载入")
		}
		return platform.Audit(tx, actor, "bastion.ssh.settings", cfg.IdentityFile)
	})
	if err == nil {
		cfg.Revision++
		h.sshConfig.Store(&cfg)
	}
	return err
}

var identityName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,47}$`)

func (h *Handler) generateIdentity(ctx context.Context, name, actor string) (sshIdentity, error) {
	var identity sshIdentity
	if !identityName.MatchString(name) {
		return identity, httpapi.NewError(400, "密钥名称需为 1–48 位字母、数字、下划线或连字符，以字母或数字开头")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	root := h.identityDirectory()
	if err := os.MkdirAll(root, 0700); err != nil {
		return identity, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return identity, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || owner.Uid != uint32(os.Geteuid()) {
		return identity, httpapi.NewError(400, "control-ssh 需为总控服务用户持有的 0700 目录，不能是符号链接")
	}
	directory, err := os.MkdirTemp(root, name+"-")
	if err != nil {
		return identity, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(directory)
		}
	}()
	path := filepath.Join(directory, "id_ed25519")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "project-alpha-control:"+name, "-f", path)
	if err = cmd.Run(); err != nil {
		return identity, httpapi.NewError(500, "生成 SSH 密钥失败，请检查 ssh-keygen 是否安装及数据目录是否可写")
	}
	identity, err = inspectIdentity(ctx, path)
	if err != nil {
		return identity, err
	}
	if err = platform.Audit(h.DB.SQL, actor, "bastion.ssh.generate", path); err != nil {
		return identity, err
	}
	complete = true
	return identity, nil
}

func (h *Handler) deleteIdentity(path, actor string) error {
	path, err := identityPath(path)
	if err != nil {
		return err
	}
	root := h.identityDirectory()
	directory := filepath.Dir(path)
	if path == "" || filepath.Base(path) != "id_ed25519" || filepath.Dir(directory) != root {
		return httpapi.NewError(400, "只能删除 control-ssh 中由总控管理的密钥，不能删除外部身份文件")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cfg, err := h.sshSettings()
	if err != nil {
		return err
	}
	if cfg.IdentityFile == path {
		return httpapi.NewError(409, "不能删除当前使用的 SSH 身份，请先切换并保存其他身份")
	}
	for _, name := range []string{root, directory} {
		info, err := os.Lstat(name)
		if os.IsNotExist(err) {
			return httpapi.NewError(404, "密钥不存在或已删除，请刷新列表")
		}
		if err != nil {
			return err
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || owner.Uid != uint32(os.Geteuid()) {
			return httpapi.NewError(400, "密钥目录需为总控服务用户持有的 0700 目录，不能是符号链接")
		}
	}
	// Only remove the two key files, never recursively remove arbitrary contents.
	files, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer files.Close()
	key, err := files.Lstat("id_ed25519")
	if os.IsNotExist(err) {
		return httpapi.NewError(404, "密钥不存在或已删除，请刷新列表")
	}
	if err != nil {
		return err
	}
	if active, err := os.Stat(cfg.IdentityFile); err == nil && os.SameFile(key, active) {
		return httpapi.NewError(409, "不能删除当前使用的 SSH 身份，请先切换并保存其他身份")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "id_ed25519" && entry.Name() != "id_ed25519.pub" {
			return httpapi.NewError(400, "密钥目录包含其他文件，拒绝删除")
		}
		info, err := files.Lstat(entry.Name())
		if err != nil {
			return err
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !ok || owner.Uid != uint32(os.Geteuid()) {
			return httpapi.NewError(400, "密钥需为总控服务用户持有的普通文件，不能是符号链接")
		}
	}
	if err = files.Remove("id_ed25519.pub"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err = files.Remove("id_ed25519"); err != nil {
		return err
	}
	if err = os.Remove(directory); err != nil {
		return err
	}
	return platform.Audit(h.DB.SQL, actor, "bastion.ssh.delete", path)
}

func (h *Handler) dispatchSSH(w http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
	switch {
	case r.URL.Path == "/api/bastion/ssh/identity" && r.Method == "DELETE":
		var req struct {
			IdentityFile string `json:"identity_file"`
		}
		if err := httpapi.DecodeBody(w, r, &req); err != nil {
			return 0, nil, err
		}
		err := h.deleteIdentity(req.IdentityFile, u.Username)
		return 200, map[string]any{"ok": true}, err
	case r.URL.Path == "/api/bastion/ssh" && r.Method == "GET":
		value, err := h.publicSSHSettings(r.Context())
		return 200, value, err
	case r.URL.Path == "/api/bastion/ssh" && r.Method == "PUT":
		var req struct {
			Revision     int64   `json:"revision"`
			IdentityFile *string `json:"identity_file"`
		}
		if err := httpapi.DecodeBody(w, r, &req); err != nil {
			return 0, nil, err
		}
		if req.Revision < 1 || req.IdentityFile == nil {
			return 0, nil, httpapi.NewError(400, "需提供总控 SSH 配置版本和已创建密钥的 identity_file")
		}
		cfg := sshSettings{Revision: req.Revision, IdentityFile: *req.IdentityFile}
		if err := h.saveSSHSettings(r.Context(), cfg, u.Username); err != nil {
			return 0, nil, err
		}
		value, err := h.publicSSHSettings(r.Context())
		return 200, value, err
	case r.URL.Path == "/api/bastion/ssh/public-key" && r.Method == "GET":
		path, err := identityPath(r.URL.Query().Get("identity_file"))
		if err != nil {
			return 0, nil, err
		}
		identity, err := h.inspectManagedIdentity(r.Context(), path)
		return 200, identity, err
	case r.URL.Path == "/api/bastion/ssh/generate" && r.Method == "POST":
		var req struct {
			Name string `json:"name"`
		}
		if err := httpapi.DecodeBody(w, r, &req); err != nil {
			return 0, nil, err
		}
		identity, err := h.generateIdentity(r.Context(), strings.TrimSpace(req.Name), u.Username)
		return 201, identity, err
	default:
		return 0, nil, httpapi.NewError(404, "接口不存在")
	}
}
