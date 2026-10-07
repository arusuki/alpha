// Package updater implements the pre-1.0, local, offline-service update tool.
package updater

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"project-alpha/internal/buildinfo"
	"project-alpha/internal/platform"
)

type options struct {
	role, directory, binDir, repo, proxy, tag string
	check, prerelease, databaseOnly           bool
	published                                 bool
	prepare                                   func(*PreparedUpdate) error
	prepared                                  *PreparedUpdate
}
type binary struct{ source, destination string }

func binaries(role string) []binary {
	result := []binary{{"project-alpha", "project-alpha"}}
	if role == "share-node" {
		result[0].destination = "project-alpha-jump"
	}
	if role == "worker" {
		result = append(result, binary{"rootless-docker", "rootless-docker"})
	}
	return append(result, binary{"alpha-updater", "alpha-updater"})
}

func Run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 1 && args[0] == "--service-protocol" {
		fmt.Fprintln(out, "2")
		return nil
	}
	if len(args) == 2 && args[0] == "_prepare" {
		return prepareService(ctx, args[1], out)
	}
	if len(args) == 2 && args[0] == "_service" {
		return service(ctx, args[1], out)
	}
	// Private protocol used only by a verified release with the service lock
	// inherited on fd 3. It never downloads, installs files or starts services.
	if len(args) > 0 && args[0] == "_migrate" {
		if len(args) != 3 {
			return fmt.Errorf("invalid migration arguments")
		}
		lock := os.NewFile(3, "service.lock")
		if lock == nil {
			return fmt.Errorf("missing inherited service lock")
		}
		defer lock.Close()
		return platform.UpgradeDatabase(args[2], args[1], lock)
	}
	p := flag.NewFlagSet("alpha-updater", flag.ContinueOnError)
	p.SetOutput(out)
	var o options
	p.StringVar(&o.role, "role", "auto", "auto（从数据库读取）、control、worker、registry 或 share-node")
	p.StringVar(&o.directory, "data-dir", os.Getenv("PROJECT_ALPHA_DATA_DIR"), "已有数据目录；默认 data；share-node 不使用数据库")
	p.StringVar(&o.binDir, "bin-dir", "", "二进制安装目录；默认本更新器所在目录，share-node 默认 /usr/local/libexec")
	p.StringVar(&o.repo, "repo", "arusuki/alpha", "GitHub owner/repo")
	p.StringVar(&o.proxy, "http-proxy", "", "HTTP/HTTPS download proxy; empty uses environment")
	p.StringVar(&o.tag, "tag", "", "Update to a specific published release tag")
	p.BoolVar(&o.check, "check", false, "仅查询最新版本和更新计划，不下载或修改文件")
	p.BoolVar(&o.prerelease, "prerelease", false, "包括预发布，按发布时间查询最新 release")
	p.BoolVar(&o.databaseOnly, "database-only", false, "离线备份并原地升级数据库，不更新二进制")
	showVersion := p.Bool("version", false, "显示更新器版本")
	p.Usage = func() {
		fmt.Fprintf(out, "用法：alpha-updater --data-dir DIR --bin-dir DIR [--role ROLE] [--check]\n离线数据库升级：alpha-updater --database-only --data-dir DIR\n目标数据库版本：%d；支持范围由构建时的 Git tag 升级窗口决定。\n先停止使用这些文件的服务，以可写入数据和二进制目录的账号运行；不自动启动/停止服务。GitHub 认证读取 GH_TOKEN 或 GITHUB_TOKEN。\n", platform.DatabaseVersion)
		p.PrintDefaults()
	}
	if err := p.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if p.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", p.Args())
	}
	if *showVersion {
		fmt.Fprintln(out, buildinfo.String("alpha-updater"))
		return nil
	}
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return fmt.Errorf("only Linux amd64/arm64 release packages are supported")
	}
	if !repoPattern.MatchString(o.repo) {
		return fmt.Errorf("--repo must be owner/repo")
	}
	switch o.role {
	case "auto", "control", "worker", "registry", "share-node":
	default:
		return fmt.Errorf("unknown role %q", o.role)
	}
	if o.databaseOnly && (o.check || o.role == "share-node") {
		return fmt.Errorf("--database-only cannot be combined with --check or share-node")
	}
	if o.role == "share-node" && o.directory != "" {
		return fmt.Errorf("share-node has no platform database; omit --data-dir and PROJECT_ALPHA_DATA_DIR")
	}
	if o.directory == "" {
		o.directory = "data"
	}
	var err error
	o.directory, err = filepath.Abs(o.directory)
	if err != nil {
		return err
	}
	if o.binDir == "" {
		if o.role == "share-node" {
			o.binDir = "/usr/local/libexec"
		} else {
			executable, e := os.Executable()
			if e != nil {
				return e
			}
			o.binDir = filepath.Dir(executable)
		}
	}
	o.binDir, err = filepath.Abs(o.binDir)
	if err != nil {
		return err
	}
	return runConfigured(ctx, o, out)
}

func runConfigured(ctx context.Context, o options, out io.Writer) error {
	if o.prepared != nil {
		return run(ctx, o, github{}, out)
	}
	token := os.Getenv("GH_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if o.proxy != "" {
		u, e := url.Parse(o.proxy)
		if e != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("invalid --http-proxy")
		}
		transport.Proxy = http.ProxyURL(u)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing non-HTTPS redirect")
		}
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
		}
		return nil
	}}
	return run(ctx, o, github{client, "https://api.github.com", o.repo, token}, out)
}

func run(ctx context.Context, o options, g github, out io.Writer) error {
	var db *platform.Database
	var lock *os.File
	var schema int
	if o.role != "share-node" {
		var err error
		db, err = platform.OpenExistingDatabase(o.directory, o.check || o.prepare != nil)
		if err != nil {
			return fmt.Errorf("open existing database: %w", err)
		}
		defer db.SQL.Close()
		if !o.check && o.prepare == nil {
			lock, err = db.LockService()
			if err != nil {
				return fmt.Errorf("stop the service before updating: %w", err)
			}
			defer lock.Close()
		}
		var role string
		schema, role, err = db.DatabaseState()
		if err != nil {
			return err
		}
		if o.role == "auto" {
			o.role = role
		}
		if role != o.role {
			return fmt.Errorf("data directory belongs to %s, not %s", role, o.role)
		}
	}
	if o.databaseOnly {
		if schema == platform.DatabaseVersion {
			fmt.Fprintf(out, "数据库已是版本 %d\n", schema)
			return nil
		}
		if _, err := backupDatabase(db, out); err != nil {
			return err
		}
		if err := platform.UpgradeDatabase(o.directory, o.role, lock); err != nil {
			return err
		}
		fmt.Fprintf(out, "数据库已原地升级：%d → %d\n", schema, platform.DatabaseVersion)
		return nil
	}
	// Serialize installations even when roles use distinct data directories.
	if !o.check {
		f, err := os.OpenFile(filepath.Join(o.binDir, ".alpha-updater.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return fmt.Errorf("another updater is using this binary directory: %w", err)
		}
	}
	pending := filepath.Join(o.binDir, ".alpha-update-pending")
	if b, err := os.ReadFile(pending); err == nil {
		return fmt.Errorf("interrupted update: inspect %s and restore the saved binaries/database before retrying:\n%s", pending, b)
	} else if !os.IsNotExist(err) {
		return err
	}
	files := binaries(o.role)
	installed, err := executableVersion(ctx, filepath.Join(o.binDir, files[0].destination), "project-alpha")
	if err != nil {
		return err
	}
	old, err := supportedVersion(installed)
	if err != nil {
		return err
	}
	var r release
	if o.prepared != nil {
		if err := o.prepared.validate(o.binDir); err != nil {
			return err
		}
		defer os.RemoveAll(o.prepared.Stage)
		if installed != o.prepared.Installed || schema != o.prepared.Schema {
			return fmt.Errorf("installed version or database changed after preparation; prepare the update again")
		}
		r.Tag = o.prepared.Tag
	} else if o.tag != "" {
		if _, err = supportedVersion(o.tag); err != nil {
			return err
		}
		if o.published && g.token == "" {
			r = g.publicRelease(o.tag, runtime.GOARCH)
		} else {
			r, err = g.byTag(ctx, o.tag, o.prerelease)
		}
	} else {
		r, err = g.latest(ctx, o.prerelease)
	}
	if err != nil {
		return err
	}
	next, err := supportedVersion(r.Tag)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "角色：%s；本地：%s；GitHub：%s；数据库：%d\n", o.role, installed, r.Tag, schema)
	if next.compare(old) < 0 {
		return fmt.Errorf("refusing downgrade from %s to %s", installed, r.Tag)
	}
	if next.compare(old) == 0 {
		fmt.Fprintln(out, "已是最新版本。若仅需升级数据库，请使用对应版本的 alpha-updater --database-only。")
		return nil
	}
	for _, f := range files {
		fmt.Fprintf(out, "更新：%s\n", filepath.Join(o.binDir, f.destination))
	}
	if o.check {
		return nil
	}
	stage := ""
	keepStage := false
	if o.prepared != nil {
		stage = o.prepared.Stage
		if err := o.prepared.verify(binaries(o.role)); err != nil {
			return err
		}
	} else {
		stage, err = os.MkdirTemp(o.binDir, ".alpha-stage-")
		if err != nil {
			return err
		}
		defer func() {
			if !keepStage {
				os.RemoveAll(stage)
			}
		}()
		names := make([]string, len(files))
		for i, f := range files {
			names[i] = f.source
		}
		if err = g.download(ctx, r, stage, runtime.GOARCH, names); err != nil {
			return err
		}
	}
	for _, name := range []string{"project-alpha", "alpha-updater"} {
		v, e := executableVersion(ctx, filepath.Join(stage, name), name)
		if e != nil {
			return e
		}
		if v != r.Tag {
			return fmt.Errorf("%s version %s does not match release %s", name, v, r.Tag)
		}
	}
	if o.role == "worker" {
		if b, e := exec.CommandContext(ctx, filepath.Join(stage, "rootless-docker"), "--help").CombinedOutput(); e != nil {
			return fmt.Errorf("rootless-docker cannot run: %w: %s", e, b)
		}
	}
	if o.prepare != nil {
		p := &PreparedUpdate{Stage: stage, Tag: r.Tag, Installed: installed, Schema: schema, Hashes: map[string]string{}}
		for _, f := range files {
			hash, err := fileHash(filepath.Join(stage, f.source))
			if err != nil {
				return err
			}
			p.Hashes[f.source] = hash
		}
		if err := o.prepare(p); err != nil {
			return err
		}
		keepStage = true
		fmt.Fprintf(out, "已下载并校验 %s，准备停服安装。\n", r.Tag)
		return nil
	}
	backup := ""
	if db != nil {
		backup, err = backupDatabase(db, out)
		if err != nil {
			return err
		}
	}
	migrate := func() error {
		if db == nil {
			return nil
		}
		cmd := exec.CommandContext(ctx, filepath.Join(o.binDir, "alpha-updater"), "_migrate", o.role, o.directory)
		cmd.ExtraFiles = []*os.File{lock}
		// Do not kill the migration process on Ctrl-C: wait for its atomic transaction
		// to finish so the parent can reliably decide whether to restore binaries.
		cmd.Cancel = func() error { return os.ErrProcessDone }
		b, e := cmd.CombinedOutput()
		if e != nil {
			var after int
			queryErr := db.SQL.QueryRow("PRAGMA user_version").Scan(&after)
			if queryErr != nil || after != schema {
				return fmt.Errorf("%w: process error %v, schema %d -> %d, query error %v: %s", errMigrationUncertain, e, schema, after, queryErr, b)
			}
			return fmt.Errorf("release database migration failed: %w: %s", e, b)
		}
		return nil
	}
	if err = install(o.binDir, stage, files, backup, migrate, out); err != nil {
		return err
	}
	fmt.Fprintf(out, "已更新至 %s；请按原配置启动 %s 服务。\n", r.Tag, o.role)
	return nil
}

func executableVersion(ctx context.Context, path, name string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("expected a regular executable: %s", path)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("query version of %s: %w: %s", path, err, b)
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 || fields[0] != name {
		return "", fmt.Errorf("unexpected version output from %s: %s", path, b)
	}
	return fields[1], nil
}
func backupDatabase(db *platform.Database, out io.Writer) (string, error) {
	path := filepath.Join(db.Directory, "platform.sqlite3.backup-"+time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := db.BackupDatabase(path); err != nil {
		return "", fmt.Errorf("backup database: %w", err)
	}
	fmt.Fprintf(out, "数据库备份：%s\n", path)
	return path, nil
}
