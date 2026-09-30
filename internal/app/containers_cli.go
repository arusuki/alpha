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
	h.UnassignOwner = func(tx *sql.Tx, username string) error {
		if _, err := tx.Exec(`UPDATE managed_containers SET owner=(SELECT owner FROM owners WHERE container_id=managed_containers.id)
 WHERE owner=? AND EXISTS(SELECT 1 FROM owners WHERE container_id=managed_containers.id AND owner<>?)`, username, username); err != nil {
			return err
		}
		// Empty overrides must survive future scans of immutable Docker labels,
		// including containers present only in historical snapshots.
		rows, err := platform.Rows(tx, `WITH candidates AS (
 SELECT json_extract(c.value,'$.id') AS id,json_extract(c.value,'$.owner') AS owner
 FROM snapshot_records r,json_each(r.metadata,'$.containers') c
 UNION ALL SELECT id,owner FROM managed_containers
 UNION ALL SELECT container_id,owner FROM owners
 ) SELECT DISTINCT c.id FROM candidates c
 LEFT JOIN managed_containers m ON m.id=c.id LEFT JOIN owners o ON o.container_id=c.id
 WHERE COALESCE(o.owner,m.owner,c.owner)=?`, username)
		if err != nil {
			return err
		}
		for _, row := range rows {
			id := row["id"].(string)
			if err = h.Owner(tx, id, ""); err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE managed_containers SET owner='' WHERE id=?", id); err != nil {
				return err
			}
		}
		return nil
	}
	return h
}

func containersCLI(ctx context.Context, args []string) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(os.Stderr, `用法：
  project-alpha containers import [选项] [容器名或完整 ID ...]

子命令：
  import  扫描并接管已有 Docker 容器，写入所在节点的 worker 数据目录

使用 project-alpha containers import --help 查看详细选项和示例。
`)
		return nil
	}
	if len(args) == 0 || args[0] != "import" {
		return fmt.Errorf("usage: project-alpha containers import [--data-dir DIR] [--endpoint unix:///path] [--base-dir /docker] [--dry-run] [CONTAINER ...]")
	}
	p := flag.NewFlagSet("project-alpha containers import", flag.ContinueOnError)
	p.Usage = func() {
		fmt.Fprint(p.Output(), `用法：
  project-alpha containers import [选项] [容器名或完整 ID ...]

扫描并接管已有 Docker 容器，无需启动 Web 服务或登录。
在容器所在节点执行，--data-dir 必须指向该节点的 worker 数据目录，不能使用总控目录。
省略容器名时扫描全部容器；指定名称或完整 ID 时，须放在所有选项之后。
导入会检查容器是否符合训练容器配置，只登记通过检查的容器；已登记项会跳过。
建议先停止 worker，导入后使用相同 --data-dir 和 --worker 启动。
--dry-run 不登记容器，但新数据目录仍会初始化数据库。

示例：
  project-alpha containers import --data-dir ./node-data --dry-run
  project-alpha containers import --data-dir ./node-data
  project-alpha containers import --data-dir ./node-data --base-dir /docker alice bob

选项：
`)
		p.PrintDefaults()
	}
	directory := os.Getenv("PROJECT_ALPHA_DATA_DIR")
	if directory == "" {
		directory = "data"
	}
	p.StringVar(&directory, "data-dir", directory, "所在节点的 worker 数据目录；默认取 PROJECT_ALPHA_DATA_DIR，否则为 data")
	var opts containers.ImportOptions
	p.StringVar(&opts.Endpoint, "endpoint", "", "Docker Unix socket；默认读取配置，新目录为 unix:///var/run/docker.sock")
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
