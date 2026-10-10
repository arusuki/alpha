package rootless

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type binding struct {
	Host       string        `json:"host"`
	Container  string        `json:"container"`
	Name       string        `json:"name"`
	SocketPath string        `json:"socket_path"`
	BootID     string        `json:"boot_id,omitempty"`
	PID        int           `json:"pid,omitempty"`
	StartedAt  string        `json:"started_at,omitempty"`
	Receipt    *mountReceipt `json:"receipt,omitempty"`
	Error      string        `json:"error,omitempty"`
}
type bindingStore struct {
	Version  int       `json:"version"`
	Bindings []binding `json:"bindings"`
}

func loadBindings(file string) (bindingStore, error) {
	var store bindingStore
	f, err := os.OpenFile(file, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return bindingStore{Version: 1, Bindings: []binding{}}, nil
	}
	if err != nil {
		return store, err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&store); err != nil {
		return store, fmt.Errorf("挂载记录无效，已保留原文件：%w", err)
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return store, errors.New("挂载记录包含多余内容，已保留原文件")
	}
	if store.Version != 1 || store.Bindings == nil {
		return store, errors.New("挂载记录格式不匹配，请使用新的管理状态目录；原文件已保留")
	}
	seen := map[string]bool{}
	for _, b := range store.Bindings {
		if validateHost(b.Host) != nil || validateContainerPath(b.SocketPath) != nil || !validContainerName(b.Container) || b.Name == "" {
			return store, errors.New("挂载记录字段无效，已保留原文件")
		}
		key := b.Host + "\x00" + b.Container + "\x00" + path.Clean(b.SocketPath)
		if seen[key] {
			return store, errors.New("挂载记录重复，已保留原文件")
		}
		seen[key] = true
		if b.Receipt != nil && (b.PID <= 1 || b.BootID == "" || b.StartedAt == "" || b.Receipt.Namespace == 0 || b.Receipt.Inode == 0) {
			return store, errors.New("挂载身份记录不完整，已保留原文件")
		}
	}
	return store, nil
}
func saveBindings(file string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(file), ".bindings-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), file); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(file))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func hostBootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b)), err
}
func runningTarget(state containerState) bool {
	return state.Running && !state.Paused && !state.Restarting && state.Pid > 1
}

// Cache only successfully persisted bytes, so failed writes remain retryable.
func (d *daemon) save() error {
	data, err := jsonBytes(d.store)
	if err != nil || bytes.Equal(data, d.savedBindings) {
		return err
	}
	if err = saveBindings(d.stateFile, data); err != nil {
		// Rename may have succeeded before the directory sync failed.
		d.savedBindings = nil
		return err
	}
	d.savedBindings = data
	return nil
}

// Match a stable container ID and its current incarnation before trusting any
// PID. A host reboot or container restart invalidates every old mount receipt.
func (d *daemon) detach(b *binding) error {
	if b.Receipt == nil {
		return nil
	}
	if b.BootID != d.bootID {
		b.Receipt = nil
		return nil
	}
	item, exists, err := d.findContainer(b.Host, b.Container)
	if err != nil {
		return err
	}
	if !exists || !item.State.Running || item.State.Pid != b.PID || item.State.StartedAt != b.StartedAt {
		b.Receipt = nil
		return nil
	}
	if err = d.manager.worker(workerRequest{Action: "detach", User: d.user, PID: b.PID, Destination: b.SocketPath, Receipt: b.Receipt}, false); err != nil {
		return err
	}
	b.Receipt = nil
	return nil
}
func (d *daemon) findContainer(host, id string) (containerInfo, bool, error) {
	// Listing distinguishes absence from Docker/transport/permission failures;
	// an unavailable daemon must never be mistaken for a removed container.
	r, err := d.manager.docker(d.user, host, "container", "ls", "--all", "--no-trunc", "--format", "{{.ID}}")
	if err != nil {
		return containerInfo{}, false, err
	}
	found := false
	for _, line := range strings.Fields(r.Out) {
		if line == id {
			found = true
			break
		}
	}
	if !found {
		return containerInfo{}, false, nil
	}
	c, err := d.manager.inspect(d.user, host, "--", id)
	return c, true, err
}
func (d *daemon) attach(b *binding, item containerInfo) error {
	if !runningTarget(item.State) {
		return errors.New("目标容器尚未运行或已暂停，等待启动事件")
	}
	source, err := socketIdentity(layout(d.user).Socket)
	if err != nil {
		return err
	}
	if source.Mode&syscall.S_IFMT != syscall.S_IFSOCK || source.Uid != d.user.UID {
		return errors.New("rootless socket 类型或所属用户不正确")
	}
	if b.Receipt != nil {
		// Repeated add is idempotent. Re-enter the worker to verify the mount,
		// but never detach an identical socket unnecessarily.
		if b.BootID == d.bootID && b.PID == item.State.Pid && b.StartedAt == item.State.StartedAt && b.Receipt.Device == uint64(source.Dev) && b.Receipt.Inode == source.Ino {
			if b.Receipt.MountID != "" {
				var mounted bool
				if err = d.manager.workerResult(workerRequest{Action: "verify-mount", User: d.user, PID: b.PID, Destination: b.SocketPath, Receipt: b.Receipt}, &mounted); err != nil {
					return err
				}
				if mounted {
					return nil
				}
			}
		} else if err = d.detach(b); err != nil {
			return err
		}
	}
	var ns syscall.Stat_t
	if err = syscall.Stat(fmt.Sprintf("/proc/%d/ns/mnt", item.State.Pid), &ns); err != nil {
		return err
	}
	b.BootID, b.PID, b.StartedAt = d.bootID, item.State.Pid, item.State.StartedAt
	b.Receipt = &mountReceipt{Namespace: ns.Ino, Device: uint64(source.Dev), Inode: source.Ino}
	// Journal intent before mounting. A crash before the final receipt is saved
	// can be recovered by exact source inode + namespace identity.
	if err = d.save(); err != nil {
		return err
	}
	receipt, err := d.mount(b, item, source)
	if err != nil {
		return err
	}
	b.Receipt = &receipt
	return nil
}
func (d *daemon) mount(b *binding, item containerInfo, before syscall.Stat_t) (mountReceipt, error) {
	m, u := d.manager, d.user
	var receipt mountReceipt
	if err := m.verifyDaemon(u); err != nil {
		return receipt, err
	}
	r, err := m.docker(u, b.Host, "info", "--format", "{{json .}}")
	if err != nil {
		return receipt, err
	}
	var rootful daemonInfo
	if err = json.Unmarshal([]byte(r.Out), &rootful); err != nil {
		return receipt, err
	}
	if isRootless(rootful) {
		return receipt, errors.New("add 的 --host 必须指向宿主机 rootful Docker。")
	}
	state := item.State
	mapping, err := m.readUIDMap(state.Pid)
	if err != nil {
		return receipt, err
	}
	if strings.Join(strings.Fields(string(mapping)), " ") != "0 0 4294967295" {
		return receipt, errors.New("暂不支持启用 userns-remap 的目标容器。")
	}
	source := layout(u).Socket
	if err = m.workerResult(workerRequest{Action: "attach", User: u, PID: state.Pid, Source: source, Destination: b.SocketPath}, &receipt); err != nil {
		return receipt, err
	}
	after, err := socketIdentity(source)
	if err != nil {
		return receipt, err
	}
	if before.Dev != after.Dev || before.Ino != after.Ino {
		return receipt, errors.New("rootless daemon 在挂载时重建了 socket，请重新执行 add。")
	}
	latest, err := m.inspect(u, b.Host, item.Id)
	if err != nil {
		return receipt, err
	}
	if latest.State.Pid != state.Pid || latest.State.StartedAt != state.StartedAt {
		return receipt, errors.New("目标容器在挂载时重启了，请重新执行 add。")
	}
	return receipt, nil
}

func (d *daemon) add(o options) error {
	item, err := d.manager.inspect(d.user, o.Host, "--", o.Container)
	if err != nil {
		return err
	}
	if item.State.Running && item.State.Pid <= 1 {
		return errors.New("目标容器 PID 无效，拒绝保存挂载")
	}
	o.SocketPath = path.Clean(o.SocketPath)
	index := -1
	for i, b := range d.store.Bindings {
		if b.Host == o.Host && b.Container == item.Id && b.SocketPath == o.SocketPath {
			index = i
			break
		}
	}
	if index == -1 {
		d.store.Bindings = append(d.store.Bindings, binding{Host: o.Host, Container: item.Id, Name: o.Container, SocketPath: o.SocketPath})
		index = len(d.store.Bindings) - 1
	}
	b := &d.store.Bindings[index]
	if !runningTarget(item.State) {
		b.Error = "等待目标容器启动或恢复运行"
		if err := d.save(); err != nil {
			return err
		}
		fmt.Fprintln(d.manager.out, "关联已保存，等待目标容器启动后挂载。")
		return nil
	}
	err = d.attach(b, item)
	b.Error = ""
	if err != nil {
		b.Error = err.Error()
	}
	saveErr := d.save()
	if err != nil {
		return err
	}
	if saveErr != nil {
		return saveErr
	}
	fmt.Fprintf(d.manager.out, "已挂载并保存关联：%s:%s。\n", o.Container, o.SocketPath)
	return nil
}
func (d *daemon) remove(o options) error {
	resolved := o.Container
	for _, b := range d.store.Bindings {
		if b.Host == o.Host && (b.Name == o.Container || b.Container == o.Container) {
			resolved = b.Container
			break
		}
	}
	if resolved == o.Container {
		// A saved name still works after deletion; other aliases are resolved
		// by Docker, never by a prefix match on a potentially reused PID.
		if item, err := d.manager.inspect(d.user, o.Host, "--", o.Container); err == nil {
			resolved = item.Id
		}
	}
	for i := range d.store.Bindings {
		b := &d.store.Bindings[i]
		if b.Host != o.Host || b.Container != resolved || b.SocketPath != path.Clean(o.SocketPath) {
			continue
		}
		if err := d.detach(b); err != nil {
			b.Error = err.Error()
			_ = d.save()
			return err
		}
		previous := d.store.Bindings
		d.store.Bindings = make([]binding, 0, len(previous)-1)
		d.store.Bindings = append(d.store.Bindings, previous[:i]...)
		d.store.Bindings = append(d.store.Bindings, previous[i+1:]...)
		if err := d.save(); err != nil {
			// Retain the association for a retry, with its completed detach.
			d.store.Bindings = previous
			return err
		}
		fmt.Fprintln(d.manager.out, "已卸载 socket 并删除关联；原有文件保留。")
		return nil
	}
	return errors.New("没有匹配的受管理挂载，未修改目标容器")
}
func (d *daemon) reconcile(host, id string) error {
	return d.reconcileBindings(func(b binding) bool {
		return (host == "" || b.Host == host) && (id == "" || b.Container == id)
	})
}

func (d *daemon) reconcileBindings(matches func(binding) bool) error {
	var errs []error
	matched := false
	for i := range d.store.Bindings {
		b := &d.store.Bindings[i]
		if !matches(*b) {
			continue
		}
		matched = true
		item, exists, err := d.findContainer(b.Host, b.Container)
		if err == nil {
			if exists {
				err = d.attach(b, item)
			} else {
				err = errors.New("目标容器已删除；新建容器需重新 add")
			}
		}
		b.Error = ""
		if err != nil {
			b.Error = err.Error()
			errs = append(errs, fmt.Errorf("%s:%s: %w", b.Name, b.SocketPath, err))
		}
	}
	if matched {
		if err := d.save(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
func (d *daemon) detachAll() error {
	var errs []error
	for i := range d.store.Bindings {
		b := &d.store.Bindings[i]
		if err := d.detach(b); err != nil {
			b.Error = err.Error()
			errs = append(errs, err)
		}
	}
	if err := d.save(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Observe without changing the stored intent. A saved receipt alone is never
// proof of a live mount: verify boot, container incarnation, socket and mount ID.
func (d *daemon) bindingStatuses() []map[string]any {
	out := []map[string]any{}
	for _, b := range d.store.Bindings {
		state, detail := "unknown", ""
		item, exists, err := d.findContainer(b.Host, b.Container)
		switch {
		case err != nil:
			detail = err.Error()
		case !exists:
			state = "missing"
			detail = "原容器已删除，未使用同名容器替代"
		case !runningTarget(item.State):
			state = "waiting"
			detail = "等待目标容器启动或恢复运行"
		case b.Receipt == nil || b.BootID != d.bootID || b.PID != item.State.Pid || b.StartedAt != item.State.StartedAt:
			state = "pending"
			detail = b.Error
		default:
			source, e := socketIdentity(layout(d.user).Socket)
			if e != nil {
				detail = e.Error()
			} else if uint64(source.Dev) != b.Receipt.Device || source.Ino != b.Receipt.Inode {
				state = "pending"
				detail = "socket 已重建，等待恢复挂载"
			} else {
				var mounted bool
				e = d.manager.workerResult(workerRequest{Action: "verify-mount", User: d.user, PID: b.PID, Destination: b.SocketPath, Receipt: b.Receipt}, &mounted)
				if e != nil {
					detail = e.Error()
				} else if mounted {
					state = "mounted"
				} else {
					state = "pending"
					detail = b.Error
				}
			}
		}
		out = append(out, map[string]any{"host": b.Host, "container": b.Container, "name": b.Name, "socket_path": b.SocketPath, "state": state, "error": detail})
	}
	return out
}
