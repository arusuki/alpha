package containers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
}
type Report struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

func (r *Report) add(name string, ok bool, reason string) {
	r.Checks = append(r.Checks, Check{name, ok, reason})
	if !ok {
		r.OK = false
	}
}
func (h *Handler) check(ctx context.Context, cfg Config, ref string) (Report, Record) {
	report := Report{OK: true, Checks: []Check{}}
	record := Record{Endpoint: cfg.Endpoint, Origin: "adopt", Initialized: true}
	daemon, err := h.daemon(ctx, cfg.Endpoint)
	if err != nil {
		report.add("Docker 连接", false, err.Error())
		return report, record
	}
	record.Daemon = daemon
	report.add("Docker 连接", true, "已确认本机 Linux daemon: "+daemon)
	c, err := h.inspect(ctx, cfg.Endpoint, ref)
	if err != nil {
		report.add("容器身份", false, err.Error())
		return report, record
	}
	record.ID = c.ID
	record.Name = strings.TrimPrefix(c.Name, "/")
	record.Fingerprint = fingerprint(c)
	report.add("容器身份", validName.MatchString(record.Name), "完整 ID: "+c.ID+"；名称: "+record.Name)
	records, err := h.records()
	if err != nil {
		report.add("管理记录", false, "无法读取管理记录: "+err.Error())
		return report, record
	}
	exists := false
	for _, r := range records {
		if r.ID == c.ID || r.Endpoint == cfg.Endpoint && r.Daemon == daemon && r.Name == record.Name {
			exists = true
		}
	}
	report.add("管理记录", !exists, "此容器 ID 或同名容器必须没有管理记录；已存在记录: "+strconv.FormatBool(exists)+"。如原容器已被替换，请先解除旧记录")
	report.add("运行状态", c.State.Running && !c.State.Paused && !c.State.Restarting && !c.State.Dead, "当前状态: "+c.State.Status+"；需正常运行才能在容器内验证 SSH 配置，请先启动或恢复容器后重试")
	report.add("主机名", c.Config.Hostname == "docker-"+record.Name, "期望 docker-"+record.Name+"，实际 "+c.Config.Hostname)
	report.add("root 用户", c.Config.User == "" || c.Config.User == "root" || c.Config.User == "0", "容器默认用户必须为 root，实际: "+strconv.Quote(c.Config.User))
	report.add("运行配置", c.Config.Tty && c.HostConfig.IpcMode == "host" && c.HostConfig.RestartPolicy.Name == "unless-stopped", fmt.Sprintf("要求 TTY=true、IPC=host、restart=unless-stopped；实际 %t / %s / %s", c.Config.Tty, c.HostConfig.IpcMode, c.HostConfig.RestartPolicy.Name))
	report.add("生命周期", !c.HostConfig.AutoRemove && !c.HostConfig.ReadonlyRootfs && !c.HostConfig.Privileged && c.HostConfig.PidMode != "host" && c.Config.Labels["com.docker.swarm.service.id"] == "", fmt.Sprintf("不支持自动删除/只读根文件系统/特权/host PID/Swarm；实际 AutoRemove=%t、ReadonlyRootfs=%t、Privileged=%t、PidMode=%q、Swarm=%t", c.HostConfig.AutoRemove, c.HostConfig.ReadonlyRootfs, c.HostConfig.Privileged, c.HostConfig.PidMode, c.Config.Labels["com.docker.swarm.service.id"] != ""))
	memlock := false
	for _, v := range c.HostConfig.Ulimits {
		if v.Name == "memlock" && v.Soft == -1 && v.Hard == -1 {
			memlock = true
		}
	}
	report.add("memlock", memlock, fmt.Sprintf("要求 memlock soft/hard 均为 -1；实际 ulimits: %+v", c.HostConfig.Ulimits))
	spec := Spec{Image: c.Config.Image, Network: c.HostConfig.NetworkMode, BaseDir: cfg.BaseDir}
	report.add("网络", spec.Network == "host" || spec.Network == "bridge", "支持 host / bridge，实际: "+spec.Network)
	expected := map[string]string{"/workspace": filepath.Join(cfg.BaseDir, record.Name, "workspace"), "/home": filepath.Join(cfg.BaseDir, record.Name, "home"), "/data": filepath.Join(cfg.BaseDir, "data")}
	report.add("挂载数量", len(c.Mounts) == 3, fmt.Sprintf("要求且仅允许 /workspace、/home、/data 三个持久化挂载，实际 %d 个；额外挂载不在接管范围，请先核对来源和用途", len(c.Mounts)))
	for _, dest := range []string{"/workspace", "/home", "/data"} {
		source := expected[dest]
		found := false
		actual := "未挂载"
		for _, m := range c.Mounts {
			if m.Destination == dest {
				actual = fmt.Sprintf("%s: %s → %s, RW=%t", m.Type, m.Source, m.Destination, m.RW)
				found = m.Type == "bind" && m.RW && m.Source == source
			}
		}
		reason := "要求可写 bind: " + source + " → " + dest + "；实际 " + actual
		report.add("挂载 "+dest, found, reason)
		pathErr := directory(source)
		report.add("宿主机目录 "+dest, pathErr == nil, reasonFor(pathErr, "目录存在、无符号链接且可访问"))
	}
	if len(c.HostConfig.DeviceRequests) == 1 {
		d := c.HostConfig.DeviceRequests[0]
		gpu := false
		for _, cap := range d.Capabilities {
			if slices.Contains(cap, "gpu") {
				gpu = true
			}
		}
		if d.Driver == "nvidia" && gpu && len(d.DeviceIDs) == 0 && (d.Count == -1 || d.Count >= 1 && d.Count <= 7) {
			spec.GPUs = strconv.Itoa(d.Count)
			if d.Count == -1 {
				spec.GPUs = "all"
			}
		}
	}
	gpuConfig, _ := json.Marshal(c.HostConfig.DeviceRequests)
	report.add("GPU", spec.GPUs != "", "要求 NVIDIA GPU 请求为 all 或 1–7 张；实际请求: "+string(gpuConfig))
	for _, v := range c.Config.Env {
		if v == "HTTP_PROXY=http://localhost:7890" {
			spec.LocalProxy = true
		}
	}
	if c.State.Running && !c.State.Paused && !c.State.Restarting {
		out, sshErr := h.run(ctx, cfg.Endpoint, []string{"exec", c.ID, "/usr/sbin/sshd", "-T"}, "")
		ports := sshPorts(out)
		ok := sshErr == nil && len(ports) == 1
		report.add("SSH 有效配置", ok, reasonFor(sshErr, fmt.Sprintf("sshd -T 必须给出唯一有效端口，实际 %v", ports)))
		if ok {
			if spec.Network == "host" {
				spec.Port = ports[0]
				report.add("SSH 端口映射", len(c.HostConfig.PortBindings) == 0, fmt.Sprintf("host 网络不应配置发布端口；实际: %v", c.HostConfig.PortBindings))
			} else {
				mapped := c.HostConfig.PortBindings["22/tcp"]
				port := 0
				valid := ports[0] == 22 && len(mapped) > 0
				for _, b := range mapped {
					p, e := strconv.Atoi(b.HostPort)
					if e != nil || p < 1 || p > 65535 || port != 0 && port != p {
						valid = false
					}
					port = p
				}
				if valid {
					spec.Port = port
				}
				report.add("SSH 端口映射", valid, fmt.Sprintf("bridge 网络要求 SSH 监听 22，且 22/tcp 映射到唯一有效宿主机端口；实际 SSH=%v，22/tcp=%v", ports, mapped))
			}
		}
	}
	record.Spec = spec
	return report, record
}
func reasonFor(err error, success string) string {
	if err != nil {
		return err.Error()
	}
	return success
}
func directory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("目录 %s 不可访问: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s 不是目录", path)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if real != path {
		return fmt.Errorf("目录 %s 含符号链接，实际为 %s；请使用直接目录挂载", path, real)
	}
	return nil
}

func sshPorts(out string) []int {
	ports := []int{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "port" {
			p, err := strconv.Atoi(fields[1])
			if err == nil && p > 0 && p <= 65535 {
				ports = append(ports, p)
			}
		}
	}
	return ports
}
