package storage

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
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
	output := "snapshots/latest.json"
	p.StringVar(&output, "output", output, "Snapshot JSON output")
	p.StringVar(&output, "o", output, "Snapshot JSON output")
	var roots, excludes stringFlags
	p.Var(&roots, "root", "Host path; repeatable")
	p.Var(&excludes, "exclude", "Excluded path; repeatable")
	p.BoolVar(&c.NoDocker, "no-docker", false, "Scan host paths without Docker")
	p.BoolVar(&c.IncludeDockerRoot, "include-docker-root", false, "Scan entire Docker data directory")
	p.StringVar(&c.ScanBackend, "scan-backend", "auto", "Scan backend: auto, host, docker (read-only helper container)")
	p.StringVar(&c.ScanMode, "scan-mode", "normal", "Scan mode: normal (up to 4 CPUs), fast (all available CPUs)")
	p.IntVar(&c.MaxDepth, "max-depth", c.MaxDepth, "Retained detail depth")
	p.IntVar(&c.MaxNodes, "max-nodes", c.MaxNodes, "Retained node budget")
	p.IntVar(&c.DockerTimeout, "docker-timeout", c.DockerTimeout, "Docker command timeout in seconds")
	p.StringVar(&c.OwnerLabel, "owner-label", c.OwnerLabel, "Docker owner label")
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
