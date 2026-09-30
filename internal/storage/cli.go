package storage

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"time"

	"project-alpha/internal/fsutil"
)

type stringFlags []string

func (s *stringFlags) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringFlags) Set(v string) error { *s = append(*s, v); return nil }
func ScanCLI(ctx context.Context, args []string) error {
	c := defaultConfig()
	p := flag.NewFlagSet("project-alpha scan", flag.ContinueOnError)
	p.SetOutput(os.Stdout)
	p.Usage = func() {
		fmt.Fprint(p.Output(), `用法：
  project-alpha scan [选项]

独立扫描 Docker 资源和显式指定的宿主路径，导出 JSON 存储快照，无需启动 Web 服务。
--root 和 --exclude 可重复指定；--no-docker 仅扫描宿主路径。

示例：
  project-alpha scan --output snapshots/latest.json
  project-alpha scan --no-docker --root /srv/data --exclude /srv/data/cache -o snapshot.json

选项：
`)
		p.PrintDefaults()
	}
	output := "snapshots/latest.json"
	p.StringVar(&output, "output", output, "快照 JSON 输出路径")
	p.StringVar(&output, "o", output, "--output 的简写")
	var roots, excludes stringFlags
	p.Var(&roots, "root", "额外扫描的宿主路径；可重复指定")
	p.Var(&excludes, "exclude", "排除的路径；可重复指定")
	p.BoolVar(&c.NoDocker, "no-docker", false, "仅扫描宿主路径，不扫描 Docker")
	p.BoolVar(&c.IncludeDockerRoot, "include-docker-root", false, "扫描整个 Docker 数据目录")
	p.StringVar(&c.ScanBackend, "scan-backend", "auto", "扫描后端：auto、host、docker（只读辅助容器）")
	p.StringVar(&c.ScanMode, "scan-mode", "normal", "扫描模式：normal（最多 4 个 CPU）、fast（全部可用 CPU）")
	p.IntVar(&c.MaxDepth, "max-depth", c.MaxDepth, "保留明细的最大深度")
	p.IntVar(&c.MaxNodes, "max-nodes", c.MaxNodes, "保留节点的数量上限")
	p.IntVar(&c.DockerTimeout, "docker-timeout", c.DockerTimeout, "Docker 命令超时秒数")
	p.StringVar(&c.OwnerLabel, "owner-label", c.OwnerLabel, "Docker 容器归属标签")
	if err := p.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if p.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", p.Args())
	}
	c.Root = append([]string{}, roots...)
	c.Exclude = append([]string{}, excludes...)
	if err := c.validate(); err != nil {
		return err
	}
	c.Exclude = append(c.Exclude, fsutil.Canonical(output))
	previousParallelism := runtime.GOMAXPROCS(c.scanParallelism())
	defer runtime.GOMAXPROCS(previousParallelism)
	lastProgress := time.Time{}
	snapshot, err := buildSnapshot(ctx, c, func(v object) error {
		if time.Since(lastProgress) >= 10*time.Second {
			lastProgress = time.Now()
			log.Printf("扫描进度：%v，条目 %v，已分配 %v B", v["path"], v["entries"], v["allocated"])
		}
		return ctx.Err()
	})
	if err != nil {
		return err
	}
	if err = atomicWrite(output, snapshot); err != nil {
		return err
	}
	log.Printf("Saved %s: %v entries, %d bytes allocated, %d scan errors", output, snapshot.Scan["visited_entries"], snapshot.Tree.Allocated, snapshot.Tree.Errors)
	if layers, ok := snapshot.Scan["writable_layers"].(object); ok && len(snapshot.Containers) > 0 {
		log.Printf("可写层：完整 %v / %v，部分统计 %v，未知 %v，权限不足 %v。Docker 逻辑大小未加进实际磁盘用量。", layers["complete"], layers["total"], layers["partial"], layers["unknown"], layers["permission_denied"])
	}
	for _, disk := range snapshot.Filesystems {
		log.Printf("文件系统 %v（%v）：已用 %v B，已扫描 %v B，尚未解释 %v B", disk["mount"], disk["device"], disk["used"], disk["scanned"], disk["unexplained"])
	}
	return nil
}
