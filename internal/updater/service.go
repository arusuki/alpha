package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ServicePlan is an internal local handoff, never accepted from a remote caller.
type ServicePlan struct {
	Format                                                 int
	Prepared                                               *PreparedUpdate
	Command, Executable, Directory, Role, Repo, Proxy, Tag string
	Arguments                                              []string
	Prerelease                                             bool
	Published                                              bool
	ServicesOnly                                           bool
}
type ServiceResult struct {
	State    string                `json:"state"`
	Error    string                `json:"error,omitempty"`
	Finished time.Time             `json:"finished_at"`
	Recovery *RecoveryInstructions `json:"recovery,omitempty"`
}
type Handoff struct{ Command, PlanPath string }

func (h *Handoff) Error() string { return "service update requested" }
func (h *Handoff) Exec() error {
	err := syscall.Exec(h.Command, []string{h.Command, "_service", h.PlanPath}, os.Environ())
	return recordServiceFailure(h.PlanPath, fmt.Errorf("启动更新器失败：%w", err))
}

func recordServiceFailure(path string, err error) error {
	result := ServiceResult{State: "failed", Error: err.Error(), Finished: time.Now().UTC()}
	if p, e := ReadServicePlan(path); e == nil {
		result = p.Result(err)
	}
	if e := WriteJSON(filepath.Join(filepath.Dir(path), "update-result.json"), result); e != nil {
		return errors.Join(err, fmt.Errorf("保存更新结果失败：%w", e))
	}
	return err
}
func ValidateTag(tag string) error { _, err := supportedVersion(tag); return err }
func Newer(next, current string) bool {
	a, e := supportedVersion(next)
	b, f := supportedVersion(current)
	return e == nil && f == nil && a.compare(b) > 0
}

// WriteJSON commits a private, crash-durable replacement without truncating the
// original file on failure. Updates settings do not change the database schema.
func WriteJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".update-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// PreparedUpdate describes a fully downloaded release. Installation consumes only
// these local files and rechecks their hashes before executing or replacing them.
type PreparedUpdate struct {
	Stage, Tag, Installed string
	Schema                int
	Hashes                map[string]string
	Containers            *containerPlan
}

func (p *PreparedUpdate) validate(binDir string) error {
	if !filepath.IsAbs(p.Stage) || filepath.Dir(p.Stage) != binDir || !strings.HasPrefix(filepath.Base(p.Stage), ".alpha-stage-") {
		return fmt.Errorf("invalid prepared update directory")
	}
	if err := ValidateTag(p.Tag); err != nil {
		return err
	}
	return ValidateTag(p.Installed)
}
func fileHash(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("expected a regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func (p *PreparedUpdate) verify(files []binary) error {
	info, err := os.Lstat(p.Stage)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("invalid prepared update directory")
	}

	for _, f := range files {
		hash, err := fileHash(filepath.Join(p.Stage, f.source))
		if err != nil {
			return err
		}
		if hash != p.Hashes[f.source] {
			return fmt.Errorf("prepared %s checksum changed; prepare the update again", f.source)
		}
	}
	return nil
}
func ReadServicePlan(path string) (*ServicePlan, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p ServicePlan
	if err = json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Format != 3 {
		return nil, fmt.Errorf("unsupported service update plan format; prepare a new update")
	}
	if !filepath.IsAbs(p.Directory) || !filepath.IsAbs(p.Executable) || filepath.Base(p.Executable) != "project-alpha" || !repoPattern.MatchString(p.Repo) {
		return nil, fmt.Errorf("invalid service update handoff")
	}
	switch p.Role {
	case "control", "worker", "registry":
	default:
		return nil, fmt.Errorf("invalid service update role")
	}
	if p.Tag != "" {
		if err := ValidateTag(p.Tag); err != nil {
			return nil, err
		}
	}
	if p.Prepared != nil {
		if err := p.Prepared.validate(filepath.Dir(p.Executable)); err != nil {
			return nil, err
		}
		if p.Tag != "" && p.Tag != p.Prepared.Tag {
			return nil, fmt.Errorf("prepared release differs from requested tag")
		}
	}
	return &p, nil
}
func (p *ServicePlan) options() options {
	return options{role: p.Role, directory: p.Directory, binDir: filepath.Dir(p.Executable), repo: p.Repo, proxy: p.Proxy, tag: p.Tag, prerelease: p.Prerelease, published: p.Published, servicesOnly: p.ServicesOnly}
}
func prepareService(ctx context.Context, path string, out io.Writer) error {
	p, err := ReadServicePlan(path)
	if err != nil {
		return err
	}
	if p.Prepared != nil {
		return fmt.Errorf("service update is already prepared")
	}
	log, err := os.OpenFile(filepath.Join(p.Directory, "update.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	o := p.options()
	o.prepare = func(prepared *PreparedUpdate) error {
		p.Prepared = prepared
		return WriteJSON(path, p)
	}
	err = runConfigured(ctx, o, io.MultiWriter(out, log))
	if err != nil {
		fmt.Fprintln(log, err)
	}
	return err
}
func service(ctx context.Context, path string, out io.Writer) error {
	p, err := ReadServicePlan(path)
	if err != nil {
		return recordServiceFailure(path, err)
	}
	log, err := os.OpenFile(filepath.Join(p.Directory, "update.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return recordServiceFailure(path, fmt.Errorf("打开更新日志失败：%w", err))
	}
	defer log.Close()
	writer := io.MultiWriter(out, log)
	o := p.options()
	o.prepared = p.Prepared
	if p.Prepared == nil {
		err = fmt.Errorf("service update has no prepared release; refusing network access after shutdown")
	} else {
		err = runConfigured(ctx, o, writer)
	}
	err = errors.Join(err, ctx.Err())
	result := p.Result(err)
	if err != nil {
		fmt.Fprintln(writer, result.Error)
	}
	if e := WriteJSON(filepath.Join(p.Directory, "update-result.json"), result); e != nil {
		return e
	}
	if _, e := os.Stat(filepath.Join(filepath.Dir(p.Executable), ".alpha-update-pending")); !os.IsNotExist(e) {
		return fmt.Errorf("update recovery required; service remains stopped; inspect update.log and .alpha-update-pending")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	restartErr := syscall.Exec(p.Executable, append([]string{p.Executable}, p.Arguments...), os.Environ())
	err = errors.Join(err, fmt.Errorf("更新后启动主程序失败：%w", restartErr))
	result = p.Result(err)
	fmt.Fprintln(writer, result.Error)
	if e := WriteJSON(filepath.Join(p.Directory, "update-result.json"), result); e != nil {
		return fmt.Errorf("%v; 保存更新结果失败：%w", err, e)
	}
	return err
}
