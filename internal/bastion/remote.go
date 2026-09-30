package bastion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/sshkeys"
)

type commandRequest struct {
	Version   int      `json:"version"`
	Operation string   `json:"operation"`
	Keys      []string `json:"keys,omitempty"`
	Key       string   `json:"key,omitempty"`
	Entry     string   `json:"entry,omitempty"`
}
type commandReply struct {
	Version    int               `json:"version"`
	ListenHost string            `json:"listen_host"`
	StatusPort int               `json:"status_port"`
	ControlURL string            `json:"control_url"`
	Keys       map[string]string `json:"keys"`
}

func netAddress(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }
func serveCommand(s keyStore, original string, in io.Reader, out io.Writer) error {
	if original != "alpha-worker cmd" {
		return fmt.Errorf("仅允许 alpha-worker cmd")
	}
	r, c, err := s.open()
	if err != nil {
		return err
	}
	defer r.Close()
	if os.Geteuid() != c.WorkerUID {
		return fmt.Errorf("管理命令必须以 alpha-worker 执行")
	}
	raw, err := io.ReadAll(io.LimitReader(in, maxKeyFile+1))
	if err != nil {
		return err
	}
	if len(raw) > maxKeyFile {
		return fmt.Errorf("请求过大")
	}
	var req commandRequest
	if err = strictJSON(raw, &req); err != nil {
		return err
	}
	if req.Version != keyFormat {
		return fmt.Errorf("管理协议版本不匹配")
	}
	var v keySnapshot
	if req.Operation == "inspect" {
		k, e := openKeys(r, c)
		if e != nil {
			return e
		}
		defer k.Close()
		v, err = loadSnapshot(k, c)
	} else {
		v, err = updateKeys(r, c, func(v *keySnapshot) error {
			switch req.Operation {
			case "ensure":
				ensurePoolKeys(v, req.Keys...)
			case "remove":
				key, e := sshkeys.Normalize(req.Key)
				if e != nil || key != req.Key {
					return fmt.Errorf("无效撤销公钥")
				}
				for id, existing := range v.Keys {
					if existing == key {
						delete(v.Keys, id)
					}
				}
			case "clean":
				if !sshkeys.ID.MatchString(req.Entry) {
					return fmt.Errorf("无效公钥条目")
				}
				delete(v.Keys, req.Entry)
			default:
				return fmt.Errorf("不支持的公钥操作")
			}
			return nil
		})
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(commandReply{Version: keyFormat, ListenHost: c.ListenHost, StatusPort: c.StatusPort, ControlURL: c.ControlURL, Keys: v.Keys})
}

// Use ordinary identity files and ssh-agent belonging to the control process.
// Preserve identity/known-host settings while disabling configured commands and
// forwards. Host keys must already be trusted by this account.
func sshArgs(s ShareNode) []string {
	return []string{"-T", "-o", "ClearAllForwardings=yes", "-o", "RemoteCommand=none", "-o", "PermitLocalCommand=no", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "PreferredAuthentications=publickey", "-o", "ConnectTimeout=8", "-o", "ConnectionAttempts=1", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no", "-p", strconv.Itoa(s.SSHPort), "-l", WorkerUser}
}

type limitedBuffer struct {
	data  []byte
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(b.data)+n > b.limit {
		return 0, fmt.Errorf("SSH 响应过大")
	}
	b.data = append(b.data, p...)
	return n, nil
}
func (h *Handler) sshCommand(ctx context.Context, s ShareNode, req commandRequest) (commandReply, error) {
	var reply commandReply
	if err := s.validate(); err != nil {
		return reply, err
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	req.Version = keyFormat
	raw, err := json.Marshal(req)
	if err != nil {
		return reply, err
	}
	args := append(sshArgs(s), s.SSHHost, "alpha-worker cmd")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	if len(raw) > maxKeyFile {
		return reply, httpapi.NewError(400, "公钥管理请求过大")
	}
	cmd.Stdin = bytes.NewReader(raw)
	output, diagnostic := &limitedBuffer{limit: maxKeyFile + 16384}, &limitedBuffer{limit: 8192}
	cmd.Stdout = output
	cmd.Stderr = diagnostic
	if err = cmd.Run(); err != nil {
		return reply, httpapi.NewError(502, fmt.Sprintf("share node %s 的 alpha-worker SSH 管理失败，请检查服务账号免密登录和 known_hosts: %s (%v)", s.Name, strings.TrimSpace(string(diagnostic.data)), err))
	}
	if err = strictJSON(output.data, &reply); err != nil {
		return reply, httpapi.NewError(502, "share node 管理响应无效，请更新跳板工具")
	}
	return reply, checkReply(s, reply)
}
func checkReply(s ShareNode, r commandReply) error {
	if r.Version != keyFormat || r.ListenHost != s.SSHHost || r.StatusPort != s.StatusPort {
		return httpapi.NewError(409, "share node 的管理协议、监听 IP 或入口端口与池配置不一致，请核对跳板初始化参数")
	}
	if _, err := proxyTarget(r.ControlURL); err != nil {
		return err
	}
	if s.ControlURL != "" && s.ControlURL != r.ControlURL {
		return httpapi.NewError(409, "share node 的总控代理地址已改变，请核对安装配置")
	}
	return validateSnapshot(keySnapshot{Version: r.Version, Keys: r.Keys}, "")
}
