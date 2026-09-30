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
	"project-alpha/internal/bastion"
	"project-alpha/internal/cluster"
	"project-alpha/internal/platform"
	"project-alpha/internal/process"
	"project-alpha/internal/registry"
	"project-alpha/internal/storage"
)

type stringFlags []string

func (s *stringFlags) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringFlags) Set(v string) error { *s = append(*s, v); return nil }
func Run(ctx context.Context, args []string) error {
	if len(args) == 2 && args[0] == "bastion" && args[1] == "install-helper" {
		return bastion.ServeInstallHelper(ctx, os.Stdin, os.Stdout)
	}
	if len(args) == 3 && args[0] == "bastion" && (args[1] == "prepare-control" || args[1] == "sync-control-keys") {
		if os.Geteuid() == 0 {
			return fmt.Errorf("总控目录必须以普通服务用户初始化")
		}
		db, err := platform.OpenDatabase(args[2], cluster.Initialize)
		if err != nil {
			return err
		}
		defer db.SQL.Close()
		id, err := db.CheckMode("control")
		if err != nil {
			return err
		}
		if args[1] == "sync-control-keys" {
			if err = bastion.NewHandler(db).SyncKeys(); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintln(os.Stdout, id)
		return err
	}
	if len(args) > 0 && args[0] == "bastion" {
		return bastion.CLI(ctx, args[1:], os.Stdout)
	}
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
  serve              启动总控（默认）、API-only worker 或公网 registry
  containers import  扫描并接管已有 Docker 容器，写入所在节点的 worker 数据目录
  bastion init       添加或重装固定 alpha-jump 跳板；日常运行免 sudo
  bastion adopt      接管已有 alpha-jump、工具和 data
  bastion release    取消接管，保留账号、工具、data 和现有授权
  bastion delete     删除 alpha-jump 账号，保留工具和 data
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
	control := p.Bool("control", false, "启动集群总控 Web 服务（默认）；与 --worker、--registry 互斥")
	worker := p.Bool("worker", false, "启动 API-only 节点；与 --control、--registry 互斥")
	registryMode := p.Bool("registry", false, "启动公网注册节点；由 control 主动建立长连接；无默认页面")
	registryTokenFile := p.String("registry-token-file", "", "registry 连接令牌文件（仅用于 --registry）；未指定时读取 PROJECT_ALPHA_REGISTRY_TOKEN，否则自动生成并保存到数据目录；在 control 网页填写相同令牌")
	regPassFile := p.String("reg-pass-file", "", "registry 的 8 位字母数字入口密码文件；否则读取 REG_PASS")
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
	if *control && *worker || *control && *registryMode || *worker && *registryMode {
		return fmt.Errorf("--control, --worker and --registry are mutually exclusive")
	}
	if !*registryMode && *regPassFile != "" {
		return fmt.Errorf("--reg-pass-file requires --registry")
	}
	if !*registryMode && *registryTokenFile != "" {
		return fmt.Errorf("--registry-token-file requires --registry; configure registry nodes in the control web interface")
	}
	registryToken, regPass := "", ""
	automaticRegistryToken := false
	if *registryMode {
		var err error
		registryToken, err = readSecret(*registryTokenFile, "PROJECT_ALPHA_REGISTRY_TOKEN")
		if err != nil {
			return fmt.Errorf("read registry token: %w", err)
		}
		automaticRegistryToken = *registryTokenFile == "" && registryToken == ""
		if !automaticRegistryToken && !cluster.ValidToken(registryToken) {
			return fmt.Errorf("registry token from --registry-token-file or PROJECT_ALPHA_REGISTRY_TOKEN must contain 32–256 non-whitespace ASCII characters")
		}
	}
	if *registryMode {
		var err error
		regPass, err = readSecret(*regPassFile, "REG_PASS")
		if err != nil {
			return err
		}
		if !registry.ValidPass(regPass) {
			return fmt.Errorf("REG_PASS must contain exactly 8 ASCII letters or digits")
		}
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
	if *registryMode {
		mode, initialize = "registry", registry.Initialize
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
			token, err = loadServiceToken(db.Directory, "worker")
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
	} else if *registryMode {
		lock, err := db.LockService()
		if err != nil {
			return err
		}
		defer lock.Close()
		if automaticRegistryToken {
			registryToken, err = loadServiceToken(db.Directory, "registry")
			if err != nil {
				return fmt.Errorf("load registry token: %w", err)
			}
		}
		frontend := registry.NewServer(db, regPass, registryToken, hosts, *secure)
		defer frontend.Hub.Close()
		handler = frontend
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
	server := &http.Server{Handler: handler, BaseContext: func(net.Listener) context.Context { return ctx }, ReadHeaderTimeout: 15 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 65536}
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
	if automaticRegistryToken {
		log.Printf("registry token (saved in data directory): %s", registryToken)
	}
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}
