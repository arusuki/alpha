package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	web "project-alpha/dist"
	"project-alpha/internal/cluster"
	"project-alpha/internal/platform"
	"project-alpha/internal/process"
	"project-alpha/internal/storage"
)

type stringFlags []string

func (s *stringFlags) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringFlags) Set(v string) error { *s = append(*s, v); return nil }
func Run(ctx context.Context, args []string) error {
	if len(args) == 1 && args[0] == "cleanup-helper" {
		return storage.ServeCleanupHelper(ctx, os.Stdin, os.Stdout)
	}
	if len(args) == 1 && args[0] == "scan-helper" {
		return storage.ServeScanHelper(ctx, os.Stdin, os.Stdout)
	}
	if len(args) > 0 && args[0] == "worker" {
		if len(args) != 4 {
			return fmt.Errorf("worker requires directory, job ID and parent PID")
		}
		parent, err := strconv.Atoi(args[3])
		if err != nil || parent <= 1 {
			return fmt.Errorf("invalid worker parent PID")
		}
		return storage.RunWorker(ctx, args[1], args[2], parent)
	}
	if len(args) > 0 && args[0] == "scan" {
		return storage.ScanCLI(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "process" {
		return process.RunCLI(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "containers" {
		return containersCLI(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	p := flag.NewFlagSet("project-alpha", flag.ContinueOnError)
	p.Usage = func() {
		fmt.Fprint(p.Output(), `用法：
  project-alpha [serve] [服务选项]
  project-alpha <子命令> [选项]

子命令：
  serve              启动总控 Web 服务（默认）或 API-only worker
  containers import  扫描并接管已有 Docker 容器，写入所在节点的 worker 数据目录
  scan               独立扫描存储并导出 JSON 快照
  process            采集容器进程或回放事件，导出 JSON 进程树

示例：
  project-alpha --control --data-dir ./control-data
  project-alpha --worker --data-dir ./node-data --host 0.0.0.0 --port 8766
  project-alpha containers import --data-dir ./node-data --dry-run
  project-alpha containers import --data-dir ./node-data

使用 project-alpha <子命令> --help 查看详细选项，例如：
  project-alpha containers import --help

服务选项（仅适用于默认启动或 serve）：
`)
		p.PrintDefaults()
	}
	directory := os.Getenv("PROJECT_ALPHA_DATA_DIR")
	if directory == "" {
		directory = "data"
	}
	p.StringVar(&directory, "data-dir", directory, "当前服务的数据目录；默认取 PROJECT_ALPHA_DATA_DIR，否则为 data；总控和各节点须使用独立目录")
	host := p.String("host", "127.0.0.1", "监听地址")
	port := p.Int("port", 8765, "HTTP 端口")
	control := p.Bool("control", false, "启动集群总控 Web 服务（默认）；与 --worker 互斥")
	worker := p.Bool("worker", false, "启动 API-only 节点；与 --control 互斥")
	workerTokenFile := p.String("worker-token-file", "", "节点令牌文件（仅用于 --worker）；未指定时读取 PROJECT_ALPHA_WORKER_TOKEN，否则自动生成并保存到数据目录")
	secure := p.Bool("secure-cookie", false, "为 HTTPS 启用 Secure 会话 Cookie")
	tetragonSocket := p.String("tetragon-socket", process.DefaultSocket, "容器进程监控使用的 Tetragon gRPC Unix socket")
	var hosts stringFlags
	p.Var(&hosts, "allowed-host", "额外允许访问的主机名；可重复指定")
	if err := p.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if p.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", p.Args())
	}
	if *port < 0 || *port > 65535 {
		return fmt.Errorf("invalid port")
	}
	if *control && *worker {
		return fmt.Errorf("--control and --worker are mutually exclusive")
	}
	mode := "control"
	initialize := cluster.Initialize
	token := ""
	automaticToken := false
	if *worker {
		mode = "worker"
		initialize = Initialize
		token = os.Getenv("PROJECT_ALPHA_WORKER_TOKEN")
		if *workerTokenFile != "" {
			raw, err := os.ReadFile(*workerTokenFile)
			if err != nil {
				return fmt.Errorf("read worker token: %w", err)
			}
			token = strings.TrimSpace(string(raw))
		} else if token == "" {
			automaticToken = true
		}
		if !automaticToken && !cluster.ValidToken(token) {
			return fmt.Errorf("worker token from --worker-token-file or PROJECT_ALPHA_WORKER_TOKEN must contain 32–256 non-whitespace ASCII characters")
		}
	} else if *workerTokenFile != "" {
		return fmt.Errorf("--worker-token-file requires --worker")
	}
	db, err := platform.OpenDatabase(directory, initialize)
	if err != nil {
		return err
	}
	defer db.SQL.Close()
	identity, err := db.CheckMode(mode)
	if err != nil {
		return err
	}
	var handler http.Handler
	if *worker {
		if automaticToken {
			token, err = loadWorkerToken(db.Directory)
			if err != nil {
				return fmt.Errorf("load worker token: %w", err)
			}
		}
		store := storage.NewStore(db)
		manager, err := storage.NewManager(store)
		if err != nil {
			return err
		}
		defer manager.Close()
		storageHandler := storage.NewHandler(store, manager)
		node := &cluster.Worker{ID: identity, Token: token, Tools: storageHandler.DispatchTools, Inventory: func() (cluster.Inventory, error) { return inventory(db) }}
		var watcher *process.Watcher
		if source, err := process.Dial(*tetragonSocket); err != nil {
			log.Printf("未启用容器进程监控：%v", err)
		} else {
			watcher = process.NewWatcher(ctx, source)
			defer watcher.Close()
		}
		node.Module = Modules{Storage: storageHandler, Containers: newContainerHandler(db), Process: process.NewHandler(watcher)}
		handler = node
	} else {
		controlHandler, err := cluster.NewControl(db)
		if err != nil {
			return err
		}
		defer controlHandler.Close()
		frontend := platform.NewServer(db, controlHandler, web.Assets, hosts, *secure)
		frontend.Control = true
		handler = frontend
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 15 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 65536}
	done := make(chan struct{})
	defer close(done)
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				server.Close()
			}
		case <-done:
		}
	}()
	log.Printf("project alpha: http://%s (%s)", listener.Addr(), mode)
	if automaticToken {
		log.Printf("worker token (saved in data directory): %s", token)
	}
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}
