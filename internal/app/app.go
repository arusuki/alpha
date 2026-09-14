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
	"project-alpha/internal/agent"
	"project-alpha/internal/platform"
	"project-alpha/internal/process"
	"project-alpha/internal/storage"
)

type stringFlags []string

func (s *stringFlags) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringFlags) Set(v string) error { *s = append(*s, v); return nil }
func Run(ctx context.Context, args []string) error {
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
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	p := flag.NewFlagSet("project-alpha", flag.ContinueOnError)
	directory := os.Getenv("PROJECT_ALPHA_DATA_DIR")
	if directory == "" {
		directory = "data"
	}
	p.StringVar(&directory, "data-dir", directory, "Persistent data directory")
	host := p.String("host", "127.0.0.1", "Listen address")
	port := p.Int("port", 8765, "HTTP port")
	secure := p.Bool("secure-cookie", false, "Use Secure session cookies for HTTPS")
	var hosts stringFlags
	p.Var(&hosts, "allowed-host", "Additional allowed hostname; repeatable")
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
	db, err := platform.OpenDatabase(directory, Initialize)
	if err != nil {
		return err
	}
	defer db.SQL.Close()
	store := storage.NewStore(db)
	manager, err := storage.NewManager(store)
	if err != nil {
		return err
	}
	defer manager.Close()
	storageHandler := storage.NewHandler(store, manager)
	agentStore := agent.NewStore(db)
	agentManager, err := agent.NewManager(agentStore, storageHandler.Service)
	if err != nil {
		return err
	}
	defer agentManager.Close()
	modules := Modules{Storage: storageHandler, Agent: agent.NewHandler(agentStore, agentManager)}
	listener, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		return err
	}
	server := &http.Server{Handler: platform.NewServer(db, modules, web.Assets, hosts, *secure), ReadHeaderTimeout: 15 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 65536}
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
	log.Printf("project alpha: http://%s", listener.Addr())
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}
