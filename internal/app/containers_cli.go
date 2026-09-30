package app

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"

	"project-alpha/internal/containers"
	"project-alpha/internal/platform"
)

func newContainerHandler(db *platform.Database) *containers.Handler {
	h := containers.NewHandler(db)
	h.Owner = func(tx *sql.Tx, id, owner string) error {
		_, err := tx.Exec("INSERT INTO owners(container_id,owner) VALUES(?,?) ON CONFLICT(container_id) DO UPDATE SET owner=excluded.owner", id, owner)
		return err
	}
	return h
}

func containersCLI(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "import" {
		return fmt.Errorf("usage: project-alpha containers import [--data-dir DIR] [--endpoint unix:///path] [--base-dir /docker] [--dry-run] [CONTAINER ...]")
	}
	p := flag.NewFlagSet("project-alpha containers import", flag.ContinueOnError)
	directory := os.Getenv("PROJECT_ALPHA_DATA_DIR")
	if directory == "" {
		directory = "data"
	}
	p.StringVar(&directory, "data-dir", directory, "平台数据目录")
	var opts containers.ImportOptions
	p.StringVar(&opts.Endpoint, "endpoint", "", "Docker Unix socket；默认读取数据目录中的配置")
	p.StringVar(&opts.BaseDir, "base-dir", "", "容器 bind 挂载根目录；默认读取配置，新目录为 /docker")
	p.BoolVar(&opts.DryRun, "dry-run", false, "仅检查，不写入容器记录、归属或配置")
	if err := p.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	opts.Containers = p.Args()
	db, err := platform.OpenDatabase(directory, Initialize)
	if err != nil {
		return err
	}
	defer db.SQL.Close()
	if _, err = db.CheckMode("worker"); err != nil {
		return err
	}
	return newContainerHandler(db).Import(ctx, opts, os.Stdout)
}
