// Package gpu observes NVIDIA devices without root or a separate agent.
package gpu

import (
	"context"
	"encoding/xml"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"project-alpha/internal/platform"
	"project-alpha/internal/procfs"
)

type Process struct {
	PID         int      `json:"pid"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Memory      *float64 `json:"memory_mib"`
	ContainerID string   `json:"container_id"`
	Container   string   `json:"container"`
	Owner       string   `json:"owner"`
	StartedAt   *float64 `json:"started_at"`
}
type Device struct {
	UUID        string    `json:"uuid"`
	Index       int       `json:"index"`
	Name        string    `json:"name"`
	Utilization *float64  `json:"utilization"`
	MemoryUsed  *float64  `json:"memory_used_mib"`
	MemoryTotal *float64  `json:"memory_total_mib"`
	Temperature *float64  `json:"temperature"`
	ComputeMode string    `json:"compute_mode"`
	MIG         bool      `json:"mig"`
	Processes   []Process `json:"processes"`
}
type smiDocument struct {
	XMLName  xml.Name `xml:"nvidia_smi_log"`
	Attached *int     `xml:"attached_gpus"`
	GPUs     []struct {
		UUID      string `xml:"uuid"`
		Name      string `xml:"product_name"`
		Util      string `xml:"utilization>gpu_util"`
		Used      string `xml:"fb_memory_usage>used"`
		Total     string `xml:"fb_memory_usage>total"`
		Temp      string `xml:"temperature>gpu_temp"`
		Mode      string `xml:"compute_mode"`
		MIG       string `xml:"mig_mode>current_mig"`
		Processes struct {
			Text  string `xml:",chardata"`
			Items []struct {
				PID    int    `xml:"pid"`
				Name   string `xml:"process_name"`
				Kind   string `xml:"type"`
				Memory string `xml:"used_memory"`
			} `xml:"process_info"`
		} `xml:"processes"`
	} `xml:"gpu"`
}

func number(raw string) *float64 {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil
	}
	n, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return nil
	}
	return &n
}
func parseSMI(raw []byte) ([]Device, error) {
	var doc smiDocument
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("无效的 nvidia-smi XML: %w", err)
	}
	if doc.Attached == nil || *doc.Attached != len(doc.GPUs) {
		return nil, fmt.Errorf("nvidia-smi GPU 数量不匹配")
	}
	out := []Device{}
	seen := map[string]bool{}
	for i, g := range doc.GPUs {
		if g.UUID == "" || seen[g.UUID] || g.Name == "" {
			return nil, fmt.Errorf("nvidia-smi 缺少有效的 GPU 标识")
		}
		seen[g.UUID] = true
		if s := strings.TrimSpace(g.Processes.Text); s != "" {
			return nil, fmt.Errorf("GPU %s 进程信息不可用: %s", g.UUID, s)
		}
		d := Device{UUID: g.UUID, Index: i, Name: g.Name, Utilization: number(g.Util), MemoryUsed: number(g.Used), MemoryTotal: number(g.Total), Temperature: number(g.Temp), ComputeMode: g.Mode, MIG: g.MIG == "Enabled", Processes: []Process{}}
		if d.Utilization != nil && *d.Utilization > 100 {
			return nil, fmt.Errorf("GPU 利用率超出范围")
		}
		for _, p := range g.Processes.Items {
			if p.PID <= 0 {
				return nil, fmt.Errorf("GPU 进程 PID 无效")
			}
			d.Processes = append(d.Processes, Process{PID: p.PID, Name: p.Name, Kind: p.Kind, Memory: number(p.Memory), Owner: "未识别"})
		}
		out = append(out, d)
	}
	return out, nil
}
func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 200 * time.Millisecond
	return cmd.Output()
}

var containerPattern = regexp.MustCompile(`(?:^|[/:-])([a-f0-9]{64})(?:\.scope)?(?:/|$)`)

// readProcess reads host procfs; a hidden or vanished PID stays unattributed.
func readProcess(root string, p *Process, boot float64) {
	dir := filepath.Join(root, strconv.Itoa(p.PID))
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return
	}
	_, ticks, ok := procfs.ParseStat(stat)
	if !ok {
		return
	}
	if boot > 0 {
		v := boot + float64(ticks)/procfs.ClockTicks
		p.StartedAt = &v
	}
	cg, err := os.ReadFile(filepath.Join(dir, "cgroup"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(cg), "\n") {
		if match := containerPattern.FindStringSubmatch(line); match != nil {
			p.ContainerID = match[1]
			p.Container = match[1][:12]
			p.Owner = "未归属容器"
			return
		}
	}
	// Non-root cgroups may be other container runtimes. Do not label them as host users.
	if strings.Contains(string(cg), "docker") || strings.Contains(string(cg), "kubepods") || strings.Contains(string(cg), "containerd") {
		return
	}
	status, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			f := strings.Fields(line)
			if len(f) > 1 {
				p.Owner = "host:uid:" + f[1]
				if u, e := user.LookupId(f[1]); e == nil {
					p.Owner = "host:" + u.Username
				}
			}
		}
	}
}
func collect(ctx context.Context, db *platform.Database) ([]Device, string, error) {
	raw, err := command(ctx, "nvidia-smi", "-q", "-x")
	if err != nil {
		return nil, "", fmt.Errorf("无法读取 NVIDIA GPU，请检查节点驱动和 nvidia-smi: %w", err)
	}
	devices, err := parseSMI(raw)
	if err != nil {
		return nil, "", err
	}
	owners, err := platform.Rows(db.SQL, "SELECT o.container_id,o.owner,COALESCE(m.name,'') AS name FROM owners o LEFT JOIN managed_containers m ON m.id=o.container_id")
	if err != nil {
		return nil, "", err
	}
	ownerMap := map[string]string{}
	names := map[string]string{}
	for _, r := range owners {
		id := r["container_id"].(string)
		ownerMap[id] = r["owner"].(string)
		names[id] = r["name"].(string)
	}
	boot := float64(0)
	if at := procfs.BootTime("/proc"); !at.IsZero() {
		boot = float64(at.Unix())
	}
	missingNames := false
	for i := range devices {
		for j := range devices[i].Processes {
			p := &devices[i].Processes[j]
			readProcess("/proc", p, boot)
			if p.ContainerID != "" && names[p.ContainerID] == "" {
				missingNames = true
			}
		}
	}
	warning := ""
	if missingNames {
		raw, e := command(ctx, "docker", "ps", "--no-trunc", "--format", "{{.ID}}\t{{.Names}}")
		if e != nil {
			warning = "Docker 名称暂不可读，容器仍按 ID 和已登记归属展示"
		} else {
			for _, line := range strings.Split(string(raw), "\n") {
				f := strings.SplitN(line, "\t", 2)
				if len(f) == 2 {
					names[f[0]] = f[1]
				}
			}
		}
	}
	for i := range devices {
		for j := range devices[i].Processes {
			p := &devices[i].Processes[j]
			if name := names[p.ContainerID]; name != "" {
				p.Container = name
			}
			if owner := ownerMap[p.ContainerID]; owner != "" {
				p.Owner = owner
			}
		}
	}
	return devices, warning, nil
}
