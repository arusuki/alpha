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
	p.StringVar(&output, "output", output, "Forest JSON output")
	p.StringVar(&output, "o", output, "Forest JSON output")
	p.StringVar(&socket, "socket", socket, "Tetragon gRPC unix socket")
	p.StringVar(&eventsFile, "events-file", eventsFile, "Replay a Tetragon JSON event dump (or - for stdin) instead of a live agent")
	p.DurationVar(&duration, "duration", duration, "Live collection window; 0 collects until interrupted")
	p.BoolVar(&includeHost, "host", includeHost, "Include processes that have no container ID")
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
