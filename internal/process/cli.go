package process

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// RunCLI collects the container process forest and writes it as JSON. It backs
// `project-alpha process`, and is the manual entry point for verifying a
// collection against a real agent or a captured event dump.
func RunCLI(ctx context.Context, args []string) error {
	output := "process-forest.json"
	socket := DefaultSocket
	eventsFile := ""
	duration := 30 * time.Second
	includeHost := false
	p := flag.NewFlagSet("project-alpha process", flag.ContinueOnError)
	p.SetOutput(os.Stdout)
	p.Usage = func() {
		fmt.Fprint(p.Output(), `用法：
  project-alpha process [选项]

从 Tetragon 实时采集容器进程，或回放 JSON 事件文件，导出 JSON 进程树。
默认采集 30 秒；--duration 0 持续采集直到中断，回放模式不使用采集时长。

示例：
  project-alpha process --duration 30s --output process-forest.json
  project-alpha process --events-file events.json -o replay.json

选项：
`)
		p.PrintDefaults()
	}
	p.StringVar(&output, "output", output, "进程树 JSON 输出路径")
	p.StringVar(&output, "o", output, "--output 的简写")
	p.StringVar(&socket, "socket", socket, "Tetragon gRPC Unix socket")
	p.StringVar(&eventsFile, "events-file", eventsFile, "回放 Tetragon JSON 事件文件；- 表示标准输入")
	p.DurationVar(&duration, "duration", duration, "实时采集时长；0 表示持续采集直到中断")
	p.BoolVar(&includeHost, "host", includeHost, "包含没有容器 ID 的宿主进程")
	if err := p.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if p.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", p.Args())
	}

	live := eventsFile == ""
	var src Source
	var err error
	if live {
		src, err = Dial(socket)
	} else {
		src, err = OpenReplay(eventsFile)
	}
	if err != nil {
		return err
	}
	defer src.Close()

	collectCtx := ctx
	if live && duration > 0 {
		var cancel context.CancelFunc
		collectCtx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}
	started := time.Now()
	builder := NewBuilder()
	stats, err := Collect(collectCtx, src, builder)
	if err != nil {
		return err
	}
	elapsed := time.Since(started)

	forest := builder.Snapshot(Options{IncludeHost: includeHost, CapturedAt: started, Duration: elapsed, Bootstrapped: stats.BootstrapOK})
	if err = writeJSON(output, forest); err != nil {
		return err
	}
	report(output, forest, stats, elapsed, live)
	return nil
}

func report(output string, forest *Forest, stats Stats, elapsed time.Duration, live bool) {
	switch {
	case stats.BootstrapError != "":
		log.Printf("未能读取 Tetragon 进程缓存，仅统计采集窗口内的事件：%v", stats.BootstrapError)
	case stats.Bootstrapped > 0:
		log.Printf("Tetragon 进程缓存：%d 个已有进程", stats.Bootstrapped)
	}
	if live {
		log.Printf("采集窗口 %s：exec %d，exit %d", elapsed.Round(time.Millisecond), stats.Exec, stats.Exit)
	} else {
		log.Printf("回放事件：exec %d，exit %d", stats.Exec, stats.Exit)
	}
	total := 0
	for _, container := range forest.Containers {
		total += container.ProcessCount
	}
	log.Printf("活动进程森林：%d 个容器，%d 个进程 → %s", len(forest.Containers), total, output)
	switch {
	case total == 0 && !live:
		log.Printf("事件中没有带容器标识的进程；这份回放可能只含主机进程，或来自未启用容器信息的 agent")
	case total == 0:
		log.Printf("未识别到容器进程；确认 Tetragon 已集成容器运行时，或用 --host 查看主机进程")
	}
	if forest.Host != nil {
		log.Printf("主机进程：%d 个（已写入输出）", forest.Host.ProcessCount)
	}
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	temp, err := os.CreateTemp(directory, ".process-forest-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err = temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
