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
	"time"

	web "project-alpha/dist"
	"project-alpha/internal/bastion"
	"project-alpha/internal/buildinfo"
	"project-alpha/internal/cluster"
	"project-alpha/internal/containers"
	"project-alpha/internal/platform"
	"project-alpha/internal/process"
	"project-alpha/internal/registry"
	"project-alpha/internal/storage"
)

type stringFlags []string

func (s *stringFlags) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringFlags) Set(v string) error { *s = append(*s, v); return nil }
func Run(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "container-permissions-helper" {
		if len(args) != 1 {
			return fmt.Errorf("container-permissions-helper does not accept arguments")
		}
		return containers.ServePermissionsHelper(ctx, os.Stdin, os.Stdout)
	}
	if len(args) > 0 && args[0] == "share-node" {
		return bastion.InitializeCLI(ctx, args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "bastion" {
		return bastion.CLI(ctx, args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "cleanup-helper" {
		if internalHelp(args[1:], os.Stdout, "cleanup-helper", "", "从标准输入接收清理请求，将结果写入标准输出。") {
			return nil
		}
		if len(args) != 1 {
			return fmt.Errorf("cleanup-helper does not accept arguments")
		}
		return storage.ServeCleanupHelper(ctx, os.Stdin, os.Stdout)
	}
	if len(args) > 0 && args[0] == "scan-helper" {
		if internalHelp(args[1:], os.Stdout, "scan-helper", "", "从标准输入接收存储扫描请求，将结果写入标准输出。") {
			return nil
		}
		if len(args) != 1 {
			return fmt.Errorf("scan-helper does not accept arguments")
		}
		return storage.ServeScanHelper(ctx, os.Stdin, os.Stdout)
	}
	if len(args) > 0 && args[0] == "worker" {
		if internalHelp(args[1:], os.Stdout, "worker", " <数据目录> <任务 ID> <父进程 PID>", "执行独立存储扫描任务；服务角色请使用 serve --worker。") {
			return nil
		}
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
	serving := len(args) > 0 && args[0] == "serve"
	if serving {
		args = args[1:]
	}
	p := flag.NewFlagSet("project-alpha", flag.ContinueOnError)
	p.SetOutput(os.Stdout)
	p.Usage = func() {
		if !serving {
			printHelp(p.Output())
			return
		}
		fmt.Fprint(p.Output(), serveHelp)
		p.PrintDefaults()
	}
	version := p.Bool("version", false, "显示版本并退出")
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
	if *version {
		fmt.Fprintln(os.Stdout, buildinfo.String("project-alpha"))
		return nil
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
		var err error
		token, err = readSecret(*workerTokenFile, "PROJECT_ALPHA_WORKER_TOKEN")
		if err != nil {
			return fmt.Errorf("read worker token: %w", err)
		}
		automaticToken = *workerTokenFile == "" && token == ""
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
