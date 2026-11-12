// Package linkhost manages the fixed root-owned helper installation and its network resources.
package linkhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"project-alpha/internal/containers"
	link "project-alpha/internal/interconnect"
	"project-alpha/internal/platform"
)

const stateDir = "/var/lib/project-alpha-interconnect"
const ledgerFormat = 1

type installation struct{ Worker, Daemon, Directory, User, Socket, Digest, PreviousDigest string }
type ownedRoute struct{ Container, Dest, Gateway, Dev, Source string }
type ledger struct {
	Format       int
	FirewallHash string
	Sources      map[string]string
	Guarded      bool
	NetworkIDs   map[string]string
	Pending      link.Config
	Endpoints    []link.Endpoint
	Routes       []ownedRoute
	Revision     int64
	Config       link.Config
}
type host struct {
	ctx     context.Context
	install installation
	state   ledger
}

func atomicFile(path string, b []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".alpha-link-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (h *host) save() error {
	b, err := json.Marshal(h.state)
	if err != nil {
		return err
	}
	return atomicFile(stateDir+"/ledger.json", b, 0600)
}
func read(path string, v any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func load(ctx context.Context) (*host, *os.File, error) {
	h := &host{ctx: ctx}
	if os.Geteuid() != 0 {
		return nil, nil, fmt.Errorf("helper 需要通过受限 sudo 调用")
	}
	lock, err := lockState()
	if err != nil {
		return nil, nil, err
	}
	err = read(stateDir+"/install.json", &h.install)
	if err == nil {
		err = read(stateDir+"/ledger.json", &h.state)
		if err != nil {
			err = fmt.Errorf("无法读取互联资源归属记录，保留已有配置: %w", err)
		} else if h.state.Format != ledgerFormat {
			err = fmt.Errorf("互联资源归属记录格式不受支持，保留已有配置")
		}
	}
	if err != nil {
		lock.Close()
		return nil, nil, err
	}
	return h, lock, nil
}
func (h *host) run(ns *os.File, input, tool string, args ...string) (string, error) {
	// Use only host binaries, a clean environment and the pinned network namespace.
	path := ""
	for _, dir := range []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		p := filepath.Join(dir, tool)
		if st, e := os.Stat(p); e == nil && st.Mode().IsRegular() {
			path = p
			break
		}
	}
	if path == "" {
		return "", fmt.Errorf("宿主机缺少 %s", tool)
	}
	if ns != nil {
		args = append([]string{"--net=/proc/self/fd/3", "--", path}, args...)
		path = "/usr/bin/nsenter"
	}
	cmd := exec.CommandContext(h.ctx, path, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "HOME=/root", "DOCKER_CONFIG=/var/empty"}
	if ns != nil {
		cmd.ExtraFiles = []*os.File{ns}
	}
	cmd.Stdin = strings.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %.600s", tool, err, stderr.String())
	}
	return string(b), nil
}
func (h *host) docker(args ...string) (string, error) {
	return h.run(nil, "", "docker", append([]string{"--host", "unix://" + h.install.Socket}, args...)...)
}
func (h *host) info() (string, error) {
	path, err := trustedSocket("unix://" + h.install.Socket)
	if err != nil {
		return "", err
	}
	if path != h.install.Socket {
		return "", fmt.Errorf("已安装的 Docker socket 身份改变")
	}
	out, err := h.docker("info", "--format", "{{json .}}")
	if err != nil {
		return "", err
	}
	var v struct {
		ID, OSType, ServerVersion string
		FirewallBackend           *struct{ Driver string }
		SecurityOptions           []string
	}
	if err = json.Unmarshal([]byte(out), &v); err != nil {
		return "", err
	}
	major, _, _ := strings.Cut(v.ServerVersion, ".")
	n, _ := strconv.Atoi(major)
	if n < 28 || v.OSType != "linux" || v.ID == "" || slices.Contains(v.SecurityOptions, "name=rootless") || (v.FirewallBackend != nil && v.FirewallBackend.Driver != "iptables") {
		return "", fmt.Errorf("需要 rootful Linux Docker 28+ 和 iptables 后端")
	}
	for _, chain := range []string{"DOCKER-USER", "DOCKER-FORWARD"} {
		if _, err = h.run(nil, "", "iptables", "-w", "5", "-C", "FORWARD", "-j", chain); err != nil {
			return "", fmt.Errorf("Docker iptables 转发未就绪: %w", err)
		}
	}
	forwarding, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil || strings.TrimSpace(string(forwarding)) != "1" {
		return "", fmt.Errorf("宿主机 IPv4 转发未启用")
	}

	return v.ID, nil
}
func (h *host) nic(addr netip.Addr) (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 || !interfaceName.MatchString(i.Name) {
			continue
		}
		addrs, e := i.Addrs()
		if e != nil {
			return "", e
		}
		for _, a := range addrs {
			p, e := netip.ParsePrefix(a.String())
			if e == nil && p.Addr() == addr {
				return i.Name, nil
			}
		}
	}
	return "", fmt.Errorf("内网 IP %s 未绑定到本机有效接口", addr)
}

func Serve(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	h, lock, err := load(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	if slices.Equal(args, []string{"guard"}) {
		if len(h.state.Endpoints) == 0 {
			return nil
		}
		return h.guard(nil)
	}
	if len(args) != 0 {
		return fmt.Errorf("无效的 helper 参数")
	}
	var req link.Request
	dec := json.NewDecoder(io.LimitReader(in, 4<<20))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&req); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return fmt.Errorf("无效的 helper 请求")
	}
	c := req.Config
	if c.Protocol != link.Protocol {
		return fmt.Errorf("helper 协议不匹配，请管理员运行 interconnect init 更新")
	}
	if c.Node != h.install.Worker {
		return fmt.Errorf("worker 身份不匹配")
	}
	if req.Operation == "apply" {
		if err = c.Validate(); err != nil {
			return err
		}
		if c.Revision < h.state.Revision || c.Revision < h.state.Pending.Revision || (c.Revision == h.state.Pending.Revision && !reflect.DeepEqual(c, h.state.Pending)) {
			return fmt.Errorf("配置版本过期或内容不一致")
		}
	}
	if req.Operation == "apply" || req.Operation == "guard" {
		if err = h.guard(nil); err != nil {
			return err
		}
	}
	if req.Operation == "guard" {
		return json.NewEncoder(out).Encode(link.Status{Protocol: link.Protocol})
	}
	db, err := platform.OpenExistingDatabase(h.install.Directory, true)
	if err != nil {
		return err
	}
	path, err := configuredSocket(db)
	db.SQL.Close()
	if err != nil {
		return err
	}
	if path != h.install.Socket {
		return fmt.Errorf("worker 的 Docker 套接字已改变，请核实节点的互联初始化")
	}
	daemon, err := h.info()
	if err != nil {
		return err
	}
	if daemon != h.install.Daemon {
		return fmt.Errorf("Docker daemon 身份已改变")
	}
	var nic string
	if req.Operation != "apply" || len(c.Endpoints) > 0 {
		if !c.Internal.Is4() || !c.Internal.IsGlobalUnicast() {
			return fmt.Errorf("容器互联需要节点配置有效的内网 IPv4 地址")
		}
		nic, err = h.nic(c.Internal)
		if err != nil {
			return err
		}
	}
	status := link.Status{Protocol: link.Protocol, Interface: nic, Daemon: daemon, Firewall: "iptables", Applied: h.state.Revision}
	switch req.Operation {
	case "check":
		if !link.ValidPool(c.Pool) {
			return fmt.Errorf("无效的 IPv4 单播地址池")
		}
		err = h.conflicts(c.Pool)
		if err == nil && len(h.state.Endpoints) > 0 {
			if h.state.Guarded {
				return fmt.Errorf("默认拒绝保护已启用，等待配置恢复")
			}
			err = h.observed()
		}
	case "apply":
		err = h.apply(c, nic, nil)
		status.Applied = h.state.Revision
	case "refresh":
		err = h.refresh(c, nic, req.Container)
		status.Applied = h.state.Revision
	case "probe":
		err = h.probe(req.Probe, c)
	default:
		return fmt.Errorf("未知的结构化网络操作")
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(status)
}

func (h *host) refresh(c link.Config, nic, container string) error {
	// During a Docker lifecycle event, retain the previously verified
	// grants between other endpoints while reconciling the changed one.
	if h.state.Guarded || !reflect.DeepEqual(c, h.state.Config) || !reflect.DeepEqual(c, h.state.Pending) {
		return fmt.Errorf("互联配置待恢复")
	}
	if !slices.ContainsFunc(c.Local(), func(e link.Endpoint) bool { return e.Container == container }) {
		return fmt.Errorf("未知本地互联端点")
	}
	for _, r := range h.state.Routes {
		if r.Container == "" && r.Dev != nic {
			return fmt.Errorf("内网接口已改变，需要完整恢复")
		}
	}
	digest, err := h.firewallDigest()
	if err != nil || digest != h.state.FirewallHash {
		return fmt.Errorf("互联防火墙需要完整恢复")
	}
	unchanged := liveConfig(c, func(e link.Endpoint) bool {
		return e.Container != container && h.state.Sources[e.IP.String()] != ""
	})
	sources := make(map[link.Ref]string)
	for _, e := range unchanged.Endpoints {
		mac := h.state.Sources[e.IP.String()]
		if mac == "" {
			return fmt.Errorf("缺少已确认的端点来源")
		}
		sources[e.Ref] = mac
	}
	keep := policy(unchanged, nic, sources)
	if err := h.guard(keep); err != nil {
		return err
	}
	return h.apply(c, nic, keep)
}

func (h *host) verifyRecord(e link.Endpoint) error {
	db, err := platform.OpenExistingDatabase(h.install.Directory, true)
	if err != nil {
		return err
	}
	defer db.SQL.Close()
	var mode, id, owner, endpoint, daemon, network, fingerprint string
	err = db.SQL.QueryRow("SELECT mode,instance_id FROM service_identity WHERE id=1").Scan(&mode, &id)
	if err != nil {
		return err
	}
	if mode != "worker" || id != e.Node {
		return fmt.Errorf("worker 数据库身份不匹配")
	}
	err = db.SQL.QueryRow("SELECT owner,endpoint,daemon,json_extract(spec,'$.network'),fingerprint FROM managed_containers WHERE id=?", e.Container).Scan(&owner, &endpoint, &daemon, &network, &fingerprint)
	if err != nil {
		return fmt.Errorf("无法确认已接管容器: %w", err)
	}
	path, err := trustedSocket(endpoint)
	if err != nil {
		return err
	}
	if owner != e.Owner || daemon != h.install.Daemon || path != h.install.Socket || network != "bridge" {
		return fmt.Errorf("容器归属、网络或 daemon 不匹配")
	}
	raw, err := h.docker("container", "inspect", e.Container)
	if err != nil {
		return err
	}
	return containers.VerifyLinkInspection(raw, e.Container, fingerprint)
}

func (h *host) guard(keep *firewallPolicy) error {
	h.state.Guarded = true
	if err := h.save(); err != nil {
		return err
	}
	return h.firewall(keep, false)
}

func lockState() (*os.File, error) {
	f, err := os.OpenFile(stateDir+"/lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Resolve the worker setting only for comparison. Privileged Docker calls use
// the canonical socket pinned in the root-owned installation record.
func configuredSocket(db *platform.Database) (string, error) {
	var endpoint string
	if err := db.SQL.QueryRow("SELECT json_extract(value,'$.endpoint') FROM container_settings WHERE id=1").Scan(&endpoint); err != nil {
		return "", err
	}
	return trustedSocket(endpoint)
}
func trustedSocket(endpoint string) (string, error) {
	path, ok := strings.CutPrefix(endpoint, "unix://")
	if !ok || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("需要本机 Docker Unix 套接字")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if st.Mode()&os.ModeSocket == 0 || st.Sys().(*syscall.Stat_t).Uid != 0 {
		return "", fmt.Errorf("需要 root 所有的本机 Docker 套接字")
	}
	for parent := filepath.Dir(real); ; parent = filepath.Dir(parent) {
		st, err = os.Stat(parent)
		if err != nil {
			return "", err
		}
		if st.Sys().(*syscall.Stat_t).Uid != 0 || st.Mode().Perm()&0022 != 0 {
			return "", fmt.Errorf("Docker 套接字目录必须由 root 管理: %s", parent)
		}
		if parent == "/" {
			break
		}
	}
	return real, nil
}
