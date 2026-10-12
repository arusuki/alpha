package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"project-alpha/internal/gpu"
	"project-alpha/internal/platform"
)

func rootlessOwnersCLI(ctx context.Context, args []string) error {
	p := flag.NewFlagSet("project-alpha containers rootless-owner", flag.ContinueOnError)
	p.SetOutput(os.Stdout)
	p.Usage = func() {
		fmt.Fprint(p.Output(), `用法：
  project-alpha containers rootless-owner --data-dir DIR --list
  project-alpha containers rootless-owner --data-dir DIR --owner USER [--all | 完整容器 ID ...]
  project-alpha containers rootless-owner --data-dir DIR --clear 完整容器 ID ...

为当前节点的 rootless 容器单独登记归属，多个子容器可关联同一使用者的主容器。
子容器跟随主容器当前归属；不会占用主容器名额，也不会重写 GPU 历史。
--clear 撤销手动关联，恢复按可信创建来源识别；已删除容器的记录也可清除。
必须使用已初始化且完成升级的 worker 数据目录；可在 worker 运行时操作。

选项：
`)
		p.PrintDefaults()
	}
	directory := os.Getenv("PROJECT_ALPHA_DATA_DIR")
	if directory == "" {
		directory = "data"
	}
	p.StringVar(&directory, "data-dir", directory, "worker 数据目录")
	owner := p.String("owner", "", "本节点主容器的当前使用者")
	all := p.Bool("all", false, "选取当前 rootless 引擎中的全部容器（含停止的容器）")
	clear := p.Bool("clear", false, "撤销所列容器的手动归属")
	list := p.Bool("list", false, "列出持久关联及当前使用者")
	dryRun := p.Bool("dry-run", false, "验证目标和归属，不写数据")
	if err := p.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	*owner = strings.TrimSpace(*owner)
	if *list {
		if *owner != "" || *clear || *all || *dryRun || p.NArg() != 0 {
			return fmt.Errorf("--list 不能与归属修改选项一起使用")
		}
	} else if (*owner != "") == *clear || *all && p.NArg() != 0 || !*all && p.NArg() == 0 {
		return fmt.Errorf("指定 --owner 或 --clear，并选择 --all 或提供完整容器 ID")
	}
	db, err := platform.OpenExistingDatabase(directory, *list)
	if err != nil {
		return err
	}
	defer db.SQL.Close()
	if _, err := db.CheckMode("worker"); err != nil {
		return err
	}
	var version int
	if err := db.SQL.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != platform.DatabaseVersion {
		return fmt.Errorf("数据库尚未升级，请先停止 worker 并运行 alpha-updater --database-only --data-dir DIR；目标版本见 alpha-updater --help")
	}
	if *list {
		rows, err := gpu.RootlessOwnerships(db)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(rows)
	}
	ids := p.Args()
	if *all {
		ids, err = gpu.RootlessContainers(ctx, db)
		if err != nil {
			return err
		}
	}
	if err := gpu.SetRootlessOwners(ctx, db, ids, *owner, "cli:rootless-owner", *dryRun); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"containers": len(ids), "owner": *owner, "cleared": *clear, "dry_run": *dryRun})
}
