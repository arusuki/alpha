package containers

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"project-alpha/internal/chatconfig"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

var cliDirectory = regexp.MustCompile(`^\.alpha-chat-[a-f0-9]{32}-[a-f0-9]{64}$`)

type cliConfig struct {
	chatconfig.Defaults
	MemberID, CommandPath string
}
type cliResult struct {
	OK        bool     `json:"ok"`
	Installed int      `json:"installed"`
	Errors    []string `json:"errors"`
}

func readChatBinary() ([]byte, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(exe), "alpha-chat")
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("worker 缺少同版本 alpha-chat，请更新 worker 发布包: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<20 || info.Size() < 4 {
		return nil, fmt.Errorf("worker alpha-chat 文件无效")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(raw, []byte{0x7f, 'E', 'L', 'F'}) {
		return nil, fmt.Errorf("worker alpha-chat 不是 Linux 可执行文件")
	}
	return raw, nil
}

func (h *Handler) configureMemberCLI(w http.ResponseWriter, r *http.Request, u platform.User, id string) (int, any, error) {
	if r.Method != "PUT" || u.Role != "admin" {
		return 0, nil, httpapi.NewError(403, "CLI 下发需要总控管理员权限")
	}
	var req struct {
		Server      string `json:"server"`
		Username    string `json:"username"`
		CommandPath string `json:"command_path"`
	}
	if err := httpapi.DecodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	cfg := cliConfig{Defaults: chatconfig.Defaults{Server: req.Server, Username: req.Username}, MemberID: id, CommandPath: req.CommandPath}
	if err := validateCLI(cfg); err != nil {
		return 0, nil, httpapi.NewError(400, err.Error())
	}
	if err := h.db.Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO member_cli_configs(username,member_id,server,command_path) VALUES(?,?,?,?) ON CONFLICT(username) DO UPDATE SET member_id=excluded.member_id,server=excluded.server,command_path=excluded.command_path`, req.Username, id, req.Server, req.CommandPath); err != nil {
			return err
		}
		return platform.Audit(tx, u.Username, "member.cli.configure", id+" / "+req.CommandPath)
	}); err != nil {
		return 0, nil, err
	}
	result := h.applyMemberCLI(r.Context(), cfg, "")
	return 200, result, nil
}

func validateCLI(cfg cliConfig) error {
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(cfg.MemberID) {
		return fmt.Errorf("使用者 ID 无效")
	}
	if err := cfg.Defaults.Validate(); err != nil {
		return err
	}
	return (chatconfig.Settings{Server: cfg.Server, CommandPath: cfg.CommandPath}).Validate()
}

func (h *Handler) applyMemberCLI(ctx context.Context, cfg cliConfig, only string) cliResult {
	result := cliResult{OK: true, Errors: []string{}}
	records, err := h.records()
	if err != nil {
		return cliResult{Errors: []string{err.Error()}}
	}
	var binary []byte
	for _, record := range records {
		if record.Owner != cfg.Username || (only != "" && record.ID != only) {
			continue
		}
		if binary == nil {
			loader := h.chatBinary
			if loader == nil {
				loader = readChatBinary
			}
			binary, err = loader()
			if err != nil {
				result.OK = false
				result.Errors = append(result.Errors, err.Error())
				break
			}
		}
		if err := h.installCLI(ctx, record, cfg, binary); err != nil {
			result.OK = false
			result.Errors = append(result.Errors, record.Name+": "+err.Error())
		} else {
			result.Installed++
		}
	}
	return result
}

func (h *Handler) reconcileCLI(ctx context.Context, only string) error {
	rows, err := platform.Rows(h.db.SQL, "SELECT member_id,username,server,command_path FROM member_cli_configs ORDER BY username")
	if err != nil {
		return err
	}
	var problems []string
	for _, row := range rows {
		cfg := cliConfig{Defaults: chatconfig.Defaults{Server: row["server"].(string), Username: row["username"].(string)}, MemberID: row["member_id"].(string), CommandPath: row["command_path"].(string)}
		if err := validateCLI(cfg); err != nil {
			return fmt.Errorf("已保存 CLI 配置无效: %w", err)
		}
		problems = append(problems, h.applyMemberCLI(ctx, cfg, only).Errors...)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

func (h *Handler) installCLI(ctx context.Context, record Record, cfg cliConfig, binary []byte) error {
	c, err := h.verify(ctx, record)
	if err != nil {
		return err
	}
	if !record.Initialized || !c.State.Running || c.State.Paused || c.State.Restarting || c.State.Dead {
		return fmt.Errorf("容器需已初始化并正常运行，启动后会重试下发")
	}
	defaults, err := json.Marshal(cfg.Defaults)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(append(append([]byte{}, binary...), defaults...))
	directory := filepath.Join(filepath.Dir(cfg.CommandPath), ".alpha-chat-"+cfg.MemberID+"-"+hex.EncodeToString(sum[:]))
	var previousPath, previousDir string
	err = h.db.SQL.QueryRow("SELECT command_path,directory FROM container_cli_installs WHERE container_id=?", record.ID).Scan(&previousPath, &previousDir)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if previousDir != "" && (!cliDirectory.MatchString(filepath.Base(previousDir)) || filepath.Dir(previousDir) != filepath.Dir(previousPath)) {
		return fmt.Errorf("已有 CLI 安装记录无效，拒绝覆盖")
	}
	if previousPath != "" {
		if err := (chatconfig.Settings{CommandPath: previousPath}).Validate(); err != nil {
			return fmt.Errorf("已有 CLI 命令路径无效: %w", err)
		}
	}
	for _, target := range []string{cfg.CommandPath, directory, previousPath, previousDir} {
		if target == "" {
			continue
		}
		for _, mount := range c.Mounts {
			root := filepath.Clean(mount.Destination)
			if root == "/" || target == root || strings.HasPrefix(target, root+"/") || strings.HasPrefix(root, target+"/") {
				return fmt.Errorf("CLI 安装路径位于容器挂载 %s 内，请选择容器自身的目录", root)
			}
		}
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, file := range []struct {
		name string
		mode int64
		data []byte
	}{{"alpha-chat", 0755, binary}, {"alpha-chat.json", 0644, defaults}} {
		if err := writer.WriteHeader(&tar.Header{Name: file.name, Mode: file.mode, Size: int64(len(file.data))}); err != nil {
			return err
		}
		if _, err := writer.Write(file.data); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	binarySum, defaultsSum := sha256.Sum256(binary), sha256.Sum256(defaults)
	out, err := h.run(ctx, record.Endpoint, []string{"exec", "-i", "--user", "0:0", "--workdir", "/", record.ID, "/bin/sh", "-c", installCLIScript, "sh", cfg.CommandPath, directory, previousPath, previousDir, hex.EncodeToString(binarySum[:]), hex.EncodeToString(defaultsSum[:])}, archive.String())
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "installed" {
		return fmt.Errorf("CLI 下发未完成: %.600s", strings.TrimSpace(out))
	}
	return h.db.Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO container_cli_installs(container_id,command_path,directory) VALUES(?,?,?) ON CONFLICT(container_id) DO UPDATE SET command_path=excluded.command_path,directory=excluded.directory`, record.ID, cfg.CommandPath, directory); err != nil {
			return err
		}
		return platform.Audit(tx, "chat-cli-control", "container.cli.install", record.ID+" / "+cfg.CommandPath)
	})
}

// All paths are validated and passed as arguments; only the two fixed archive
// entries are extracted into a fresh directory. A symlink switches the executable
// and its matching defaults together, so interruption cannot mix identities.
const installCLIScript = `set -efu
fail() { printf '%s\n' "$1"; exit 0; }
dest=$1; release=$2; oldpath=$3; olddir=$4; binarysum=$5; configsum=$6
if [ -n "$oldpath" ]; then
 oldparent=${oldpath%/*}; current=
 oldifs=$IFS; IFS=/; set -- ${oldparent#/}; IFS=$oldifs
 for part do
  current=$current/$part
  [ ! -L "$current" ] || fail '原安装目录包含符号链接，拒绝清理'
 done
fi
parent=${dest%/*}; current=
oldifs=$IFS; IFS=/; set -- ${parent#/}; IFS=$oldifs
for part do
 current=$current/$part
 [ ! -L "$current" ] || fail '安装目录包含符号链接，拒绝写入'
 if [ ! -d "$current" ]; then mkdir "$current" || fail '无法创建安装目录'; fi
done
if [ -e "$dest" ] || [ -L "$dest" ]; then
 [ -L "$dest" ] || fail '命令路径已被其他文件占用'
 link=$(readlink "$dest")
 if [ "$link" != "$release/alpha-chat" ] && { [ -z "$olddir" ] || [ "$link" != "$olddir/alpha-chat" ]; }; then fail '命令路径不是本服务管理的链接'; fi
fi
[ ! -L "$release" ] || fail '程序目录不能是符号链接'
tmp=$(mktemp -d "$parent/.alpha-chat-tmp.XXXXXXXX") || fail '无法创建临时目录'
trap 'if [ -n "$tmp" ]; then rm -f "$tmp/alpha-chat" "$tmp/alpha-chat.json" "$tmp/link"; rmdir "$tmp" 2>/dev/null || true; fi' EXIT
chmod 755 "$tmp"
tar -xf - -C "$tmp" || fail '无法读取 CLI 安装包'
[ "$(sha256sum "$tmp/alpha-chat" | cut -d ' ' -f 1)" = "$binarysum" ] || fail 'CLI 校验失败'
[ "$(sha256sum "$tmp/alpha-chat.json" | cut -d ' ' -f 1)" = "$configsum" ] || fail 'CLI 配置校验失败'
"$tmp/alpha-chat" --version >/dev/null || fail 'CLI 无法在容器内运行，请检查架构和 worker 发布包'
if [ -e "$release" ]; then
 [ -d "$release" ] && [ ! -L "$release/alpha-chat" ] && [ ! -L "$release/alpha-chat.json" ] || fail '已有程序目录无效'
 [ "$(sha256sum "$release/alpha-chat" | cut -d ' ' -f 1)" = "$binarysum" ] && [ "$(sha256sum "$release/alpha-chat.json" | cut -d ' ' -f 1)" = "$configsum" ] || fail '已有 CLI 文件已修改，请核对安装目录'
else
 mv "$tmp" "$release" || fail '无法发布 CLI 程序目录'
 tmp=$(mktemp -d "$parent/.alpha-chat-tmp.XXXXXXXX") || fail '无法创建链接临时目录'
fi
ln -s "$release/alpha-chat" "$tmp/link"
mv -Tf "$tmp/link" "$dest" || fail '无法更新 CLI 命令'
if [ -n "$oldpath" ] && [ "$oldpath" != "$dest" ] && [ -L "$oldpath" ] && [ "$(readlink "$oldpath")" = "$olddir/alpha-chat" ]; then rm "$oldpath"; fi
if [ -n "$olddir" ] && [ "$olddir" != "$release" ] && [ -d "$olddir" ] && [ ! -L "$olddir" ]; then
 rm -f "$olddir/alpha-chat" "$olddir/alpha-chat.json"
 rmdir "$olddir" 2>/dev/null || true
fi
printf 'installed\n'
`
