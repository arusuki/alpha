package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ServicePlan is an internal local handoff, never accepted from a remote caller.
type ServicePlan struct {
	Command, Executable, Directory, Role, Repo, Proxy, Tag string
	Arguments                                              []string
	Prerelease                                             bool
}
type ServiceResult struct {
	State    string    `json:"state"`
	Error    string    `json:"error,omitempty"`
	Finished time.Time `json:"finished_at"`
}
type Handoff struct{ Command, PlanPath string }

func (h *Handoff) Error() string { return "service update requested" }
func (h *Handoff) Exec() error {
	return syscall.Exec(h.Command, []string{h.Command, "_service", h.PlanPath}, os.Environ())
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
func service(ctx context.Context, path string, out io.Writer) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var p ServicePlan
	if err = json.Unmarshal(raw, &p); err != nil {
		return err
	}
	if !filepath.IsAbs(p.Directory) || !filepath.IsAbs(p.Executable) || p.Executable != filepath.Join(filepath.Dir(p.Executable), "project-alpha") {
		return fmt.Errorf("invalid service update handoff")
	}
	log, err := os.OpenFile(filepath.Join(p.Directory, "update.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	writer := io.MultiWriter(out, log)
	args := []string{"--role", p.Role, "--data-dir", p.Directory, "--bin-dir", filepath.Dir(p.Executable), "--repo", p.Repo, "--http-proxy", p.Proxy}
	if p.Prerelease {
		args = append(args, "--prerelease")
	}
	if p.Tag != "" {
		args = append(args, "--tag", p.Tag)
	}
	result := ServiceResult{State: "completed"}
	if err = Run(ctx, args, writer); err != nil {
		result.State = "failed"
		result.Error = err.Error()
		fmt.Fprintln(writer, err)
	}
	result.Finished = time.Now().UTC()
	if e := WriteJSON(filepath.Join(p.Directory, "update-result.json"), result); e != nil {
		return e
	}
	if _, e := os.Stat(filepath.Join(filepath.Dir(p.Executable), ".alpha-update-pending")); !os.IsNotExist(e) {
		return fmt.Errorf("update recovery required; service remains stopped; inspect update.log and .alpha-update-pending")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return syscall.Exec(p.Executable, append([]string{p.Executable}, p.Arguments...), os.Environ())
}
