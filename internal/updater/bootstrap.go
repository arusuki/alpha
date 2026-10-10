package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// bootstrapRequest is the fixed v1 boundary between releases. It identifies a
// verified archive and installation, never a release-specific file list or
// container plan. Credentials stay in the caller's environment/service plan.
type bootstrapRequest struct {
	Format                             int
	Role, Directory, BinDir, Repo, Tag string
	ArchiveHash, ServicePlan, Stage    string
	Command                            string
	ServicesOnly                       bool
}

func bootstrapRelease(ctx context.Context, o options, r release, g github, out io.Writer) error {
	dir, err := os.MkdirTemp(o.binDir, ".alpha-bootstrap-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	// The bootstrap knows only the archive convention and updater executable.
	// In particular it must not extract or decode updater-services.json.
	if err := g.download(ctx, r, dir, runtime.GOARCH, []string{"alpha-updater"}); err != nil {
		return err
	}
	command := filepath.Join(dir, "alpha-updater")
	version, err := executableVersion(ctx, command, "alpha-updater")
	if err != nil {
		return err
	}
	if version != r.Tag {
		return fmt.Errorf("alpha-updater version %s does not match release %s", version, r.Tag)
	}
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	protocol, err := exec.CommandContext(probe, command, "--bootstrap-protocol").Output()
	cancel()
	if err != nil || strings.TrimSpace(string(protocol)) != "1" {
		return fmt.Errorf("release updater does not support bootstrap protocol v1; nothing installed")
	}
	hash, err := fileHash(filepath.Join(dir, "release.tar.gz"))
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(o.binDir, ".alpha-stage-")
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(stage)
		}
	}()
	req := bootstrapRequest{Format: 1, Role: o.role, Directory: o.directory, BinDir: o.binDir, Repo: g.repo, Tag: r.Tag, ArchiveHash: hash, ServicePlan: o.servicePlan, ServicesOnly: o.servicesOnly, Stage: stage, Command: o.command}
	path := filepath.Join(dir, "request.json")
	if err := WriteJSON(path, req); err != nil {
		return err
	}
	fmt.Fprintf(out, "已校验目标版本 updater %s，由它接管准备和安装。\n", r.Tag)
	cmd := exec.CommandContext(ctx, command, "_release", path)
	cmd.Stdout = out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// Let the target finish rollback on cancellation. If the bootstrap is
	// killed, its child still receives cancellation instead of becoming orphaned.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	err = cmd.Run()
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		// A cancellation can arrive just after the target saved preparation.
		// The caller will not hand off in this case, so discard that private stage.
		if o.servicePlan != "" {
			if p, e := ReadServicePlan(o.servicePlan); e == nil && p.Prepared != nil && p.Prepared.Stage == stage {
				p.Prepared = nil
				if e := WriteJSON(o.servicePlan, p); e != nil {
					return fmt.Errorf("release updater failed: %w; clear preparation: %v", err, e)
				}
			}
		}
		return fmt.Errorf("release updater failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if o.servicePlan != "" {
		p, err := ReadServicePlan(o.servicePlan)
		if err != nil {
			return err
		}
		if p.Prepared != nil {
			if p.Prepared.Stage != stage || p.Prepared.Tag != r.Tag {
				return fmt.Errorf("release updater returned a different preparation")
			}
			keep = true
		}
	}
	return nil
}

// runRelease is executed only by the target updater. The archive is local and
// rechecked before extracting this release's payload; no second download occurs.
func runRelease(ctx context.Context, path string, out io.Writer) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var req bootstrapRequest
	if err = json.Unmarshal(raw, &req); err != nil {
		return err
	}
	if req.Format != 1 || !filepath.IsAbs(req.BinDir) || !filepath.IsAbs(req.Directory) || !repoPattern.MatchString(req.Repo) || ValidateTag(req.Tag) != nil {
		return fmt.Errorf("invalid release bootstrap request")
	}
	switch req.Role {
	case "control", "worker", "registry", "share-node":
	default:
		return fmt.Errorf("invalid release bootstrap role")
	}
	bootstrap := filepath.Dir(path)
	if filepath.Dir(bootstrap) != req.BinDir || !strings.HasPrefix(filepath.Base(bootstrap), ".alpha-bootstrap-") {
		return fmt.Errorf("invalid release bootstrap directory")
	}
	archive := filepath.Join(bootstrap, "release.tar.gz")
	hash, err := fileHash(archive)
	if err != nil {
		return err
	}
	if hash != req.ArchiveHash {
		return fmt.Errorf("release archive checksum changed; nothing installed")
	}
	o := options{role: req.Role, directory: req.Directory, binDir: req.BinDir, repo: req.Repo, tag: req.Tag, servicesOnly: req.ServicesOnly, command: req.Command}
	var plan *ServicePlan
	if req.ServicePlan != "" {
		if req.ServicePlan != filepath.Join(req.Directory, "update-service.json") {
			return fmt.Errorf("invalid bootstrap service plan path")
		}
		plan, err = ReadServicePlan(req.ServicePlan)
		if err != nil {
			return err
		}
		if plan.Prepared != nil || plan.Command != req.Command || plan.Directory != req.Directory || filepath.Dir(plan.Executable) != req.BinDir || plan.Role != req.Role || plan.Repo != req.Repo || plan.Tag != "" && plan.Tag != req.Tag || plan.ServicesOnly && !req.ServicesOnly {
			return fmt.Errorf("bootstrap service plan changed; prepare again")
		}
	}
	stage := req.Stage
	if !filepath.IsAbs(stage) || filepath.Dir(stage) != req.BinDir || !strings.HasPrefix(filepath.Base(stage), ".alpha-stage-") {
		return fmt.Errorf("invalid bootstrap payload directory")
	}
	info, err := os.Lstat(stage)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("invalid bootstrap payload directory")
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(stage)
		}
	}()
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	var names []string
	for _, file := range releaseFiles(req.Role) {
		names = append(names, file.source)
	}
	if err := extract(f, stage, "project-alpha_"+req.Tag+"_linux_"+runtime.GOARCH, names); err != nil {
		return err
	}
	// The offline migration validates services from the exact same archive.
	// A hard link keeps it available after the download bootstrap is removed.
	if err := os.Link(archive, filepath.Join(stage, "release.tar.gz")); err != nil {
		return err
	}
	if err := syncDirectory(stage); err != nil {
		return err
	}
	o.downloaded = stage
	if plan != nil {
		o.prepare = func(p *PreparedUpdate) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			plan.Prepared = p
			if err := WriteJSON(req.ServicePlan, plan); err != nil {
				return err
			}
			keep = true
			return nil
		}
	}
	return run(ctx, o, github{}, out)
}
