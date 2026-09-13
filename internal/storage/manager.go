package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

type Manager struct {
	Agent     *AgentManager
	db        *Store
	mu        sync.Mutex
	lockfile  *os.File
	process   *exec.Cmd
	jobID     string
	logfile   *os.File
	done      chan error
	stop      chan struct{}
	stopped   chan struct{}
	closeOnce sync.Once
	closed    bool
	command   func(string, string) *exec.Cmd
}

func NewManager(db *Store) (*Manager, error) {
	lock, err := os.OpenFile(filepath.Join(db.Directory, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("此数据目录已有管理服务正在运行: %w", err)
	}
	m := &Manager{db: db, lockfile: lock, stop: make(chan struct{}), stopped: make(chan struct{})}
	leases, _ := filepath.Glob(filepath.Join(db.Directory, "results", "*", "scan-helper.json"))
	for _, lease := range leases {
		if err := cleanupHelperLease(lease); err != nil {
			log.Printf("cleanup previous scan helper: %v", err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		lock.Close()
		return nil, err
	}
	m.command = func(directory, id string) *exec.Cmd {
		return exec.Command(executable, "worker", directory, id, strconv.Itoa(os.Getpid()))
	}
	if _, err = db.SQL.Exec("UPDATE jobs SET status='interrupted',finished_at=?,error='服务重启，之前的扫描未完成' WHERE status IN ('queued','running','cancelling')", platform.Now()); err != nil {
		lock.Close()
		return nil, err
	}
	m.Agent, err = newAgentManager(db, m)
	if err != nil {
		lock.Close()
		return nil, err
	}
	go m.loop()
	return m, nil
}
func (m *Manager) Start(actor, trigger string) (object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked(actor, trigger)
}
func (m *Manager) startLocked(actor, trigger string) (object, error) {
	config, err := m.db.config()
	if err != nil {
		return nil, err
	}
	return m.startPlanLocked(actor, trigger, scanPlan{Config: config.Value})
}

// Analysis uses the same isolated worker, cancellation and single-scan lock.
type scanPlan struct {
	Config
	AnalysisPath    string `json:"analysis_path,omitempty"`
	BaseJobID       string `json:"base_job_id,omitempty"`
	BaseRevision    int64  `json:"base_revision,omitempty"`
	IncrementalPath string `json:"incremental_path,omitempty"`
}

func (m *Manager) startPlan(actor, trigger string, plan scanPlan) (object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startPlanLocked(actor, trigger, plan)
}

func (m *Manager) startPlanLocked(actor, trigger string, plan scanPlan) (object, error) {
	if m.closed {
		return nil, httpapi.NewError(503, "服务正在关闭")
	}
	if err := m.reapLocked(); err != nil {
		return nil, err
	}
	if m.process != nil {
		return nil, httpapi.NewError(409, "已有扫描正在执行，请等待完成或取消当前任务")
	}
	// Apply the saved scan mode when queuing, while retaining the plan's scan scope.
	settings, err := m.db.config()
	if err != nil {
		return nil, err
	}
	plan.ScanMode = settings.Value.ScanMode
	id := platform.RandomHex(16)
	err = m.db.Transaction(func(tx *sql.Tx) error {
		if plan.BaseJobID != "" {
			var worker string
			err := tx.QueryRow("SELECT id FROM jobs WHERE trigger='incremental' AND json_extract(config,'$.base_job_id')=? ORDER BY created_at DESC LIMIT 1", plan.BaseJobID).Scan(&worker)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if err == nil {
				id = worker
				_, err = tx.Exec("UPDATE jobs SET status='queued',created_by=?,created_at=?,started_at=NULL,finished_at=NULL,error=NULL,progress='{}',allocated=NULL,files=NULL,warnings=NULL,config=? WHERE id=?", actor, platform.Now(), httpapi.JSONText(plan), id)
				return err
			}
		}
		_, err := tx.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config) VALUES(?,?,?,?,?,?)", id, "queued", trigger, actor, platform.Now(), httpapi.JSONText(plan))
		if err != nil {
			if platform.IsConstraint(err) {
				return httpapi.NewError(409, "已有扫描正在执行，请等待完成或取消当前任务")
			}
			return err
		}
		return platform.Audit(tx, actor, "scan.start", id+" / "+trigger)
	})
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(m.db.Directory, "results", id)
	fail := func(cause error) (object, error) {
		if m.logfile != nil {
			m.logfile.Close()
			m.logfile = nil
		}
		_, updateErr := m.db.SQL.Exec("UPDATE jobs SET status='failed',finished_at=?,error=? WHERE id=?", platform.Now(), cause.Error(), id)
		if updateErr != nil {
			return nil, updateErr
		}
		return nil, httpapi.NewError(500, "无法启动扫描进程")
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return fail(err)
	}
	m.logfile, err = os.OpenFile(filepath.Join(directory, "worker.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fail(err)
	}
	cmd := m.command(m.db.Directory, id)
	// Apply to this job's isolated process before runtime initialization, covering
	// host scans, discovery and summarization without changing the web service.
	cmd.Env = append(cmd.Environ(), "GOMAXPROCS="+strconv.Itoa(plan.Config.scanParallelism()))
	cmd.Stdout = m.logfile
	cmd.Stderr = m.logfile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		return fail(err)
	}
	m.process = cmd
	m.jobID = id
	m.done = make(chan error, 1)
	done := m.done
	go func() { done <- cmd.Wait() }()
	return m.db.job(id)
}
func (m *Manager) reapLocked() error {
	if m.process == nil {
		return nil
	}
	select {
	case err := <-m.done:
		return m.finishLocked(err)
	default:
		return nil
	}
}
func (m *Manager) finishLocked(waitErr error) error {
	// Reap descendants left by a failed or forcibly terminated worker.
	if err := syscall.Kill(-m.process.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		log.Printf("stop worker descendants: %v", err)
	}
	if err := cleanupHelperLease(filepath.Join(m.db.Directory, "results", m.jobID, "scan-helper.json")); err != nil {
		log.Printf("cleanup scan helper: %v", err)
	}
	job, err := m.db.job(m.jobID)
	if err != nil {
		m.done <- waitErr
		return err
	}
	status := job["status"].(string)
	if activeStatus(status) {
		status = "failed"
		message := fmt.Sprintf("扫描进程异常退出（%v）", waitErr)
		if job["status"] == "cancelling" {
			status = "cancelled"
			message = "任务已取消"
		}
		if _, err = m.db.SQL.Exec("UPDATE jobs SET status=?,finished_at=?,error=? WHERE id=?", status, platform.Now(), message, m.jobID); err != nil {
			m.done <- waitErr
			return err
		}
	}
	if m.logfile != nil {
		m.logfile.Close()
		m.logfile = nil
	}
	if status != "completed" {
		for _, pattern := range []string{"snapshot.json", ".scan-*.json"} {
			paths, _ := filepath.Glob(filepath.Join(m.db.Directory, "results", m.jobID, pattern))
			for _, p := range paths {
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					log.Printf("remove unpublished result: %v", err)
				}
			}
		}
	}
	m.process = nil
	m.jobID = ""
	m.done = nil
	return nil
}
func activeStatus(s string) bool { return s == "queued" || s == "running" || s == "cancelling" }
func (m *Manager) Cancel(id, actor string) (object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelLocked(id, actor)
}
func (m *Manager) cancelLocked(id, actor string) (object, error) {
	if err := m.reapLocked(); err != nil {
		return nil, err
	}
	if m.process == nil || m.jobID != id {
		return nil, httpapi.NewError(409, "任务已结束，无法取消")
	}
	err := m.db.Transaction(func(tx *sql.Tx) error {
		r, err := tx.Exec("UPDATE jobs SET status='cancelling' WHERE id=? AND status IN ('queued','running')", id)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return httpapi.NewError(409, "任务已结束或正在取消")
		}
		return platform.Audit(tx, actor, "scan.cancel", id)
	})
	if err != nil {
		return nil, err
	}
	if err = syscall.Kill(-m.process.Process.Pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return nil, err
	}
	select {
	case waitErr := <-m.done:
		err = m.finishLocked(waitErr)
	case <-time.After(2 * time.Second):
		if killErr := syscall.Kill(-m.process.Process.Pid, syscall.SIGKILL); killErr != nil && killErr != syscall.ESRCH {
			return nil, killErr
		}
		err = m.finishLocked(<-m.done)
	}
	if err != nil {
		return nil, err
	}
	return m.db.job(id)
}
func (m *Manager) tick() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.reapLocked(); err != nil {
		return err
	}
	if m.closed || m.process != nil {
		return nil
	}
	configured, err := m.db.Configured()
	if err != nil || !configured {
		return err
	}
	s, err := m.db.config()
	if err != nil || s.Value.IntervalMinutes == 0 {
		return err
	}
	var last sql.NullFloat64
	if err = m.db.SQL.QueryRow("SELECT max(coalesce(finished_at,created_at)) FROM jobs WHERE trigger NOT IN ('agent-detail','incremental')").Scan(&last); err != nil {
		return err
	}
	if !last.Valid || platform.Now()-last.Float64 >= float64(s.Value.IntervalMinutes*60) {
		_, err = m.startLocked("scheduler", "scheduled")
		return err
	}
	return nil
}
func (m *Manager) loop() {
	defer close(m.stopped)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			if err := m.Agent.flushCompletion(); err != nil {
				log.Printf("Agent completion: %v; will retry", err)
			}
			if err := m.tick(); err != nil {
				log.Printf("Scan manager: %v", err)
			}
		}
	}
}
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		if m.Agent != nil {
			m.Agent.Close()
		}
		close(m.stop)
		<-m.stopped
		m.mu.Lock()
		defer m.mu.Unlock()
		m.closed = true
		if m.process != nil {
			if _, err := m.cancelLocked(m.jobID, "service-shutdown"); err != nil {
				log.Printf("stop scan: %v", err)
				if m.process != nil {
					syscall.Kill(-m.process.Process.Pid, syscall.SIGKILL)
					m.finishLocked(<-m.done)
				}
			}
		}
		syscall.Flock(int(m.lockfile.Fd()), syscall.LOCK_UN)
		m.lockfile.Close()
	})
}
func RunWorker(ctx context.Context, directory, id string, parent int) error {
	if syscall.Getpgrp() != os.Getpid() {
		return fmt.Errorf("worker must run in its own process group")
	}
	ctx = context.WithValue(ctx, workerProcessGroup{}, true)
	ctx = context.WithValue(ctx, helperLeaseKey{}, filepath.Join(directory, "results", id, "scan-helper.json"))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			if os.Getppid() != parent {
				// The group is platform-owned; also stop any Docker descendants.
				syscall.Kill(-os.Getpid(), syscall.SIGKILL)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	base, err := platform.OpenDatabase(directory, Initialize)
	if err != nil {
		return err
	}
	defer base.SQL.Close()
	db := NewStore(base)
	var raw string
	if err = db.SQL.QueryRow("SELECT config FROM jobs WHERE id=?", id).Scan(&raw); err != nil {
		return err
	}
	var plan scanPlan
	if err = json.Unmarshal([]byte(raw), &plan); err != nil {
		return err
	}
	c := plan.Config
	c.Exclude = append(c.Exclude, db.Directory)
	r, err := db.SQL.Exec("UPDATE jobs SET status='running',started_at=? WHERE id=? AND status='queued'", platform.Now(), id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	lastProgress := object{}
	progress := func(v object) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := db.SQL.Exec("UPDATE jobs SET progress=? WHERE id=? AND status='running'", httpapi.JSONText(v), id)
		lastProgress = v
		return err
	}
	var result *Snapshot
	if plan.BaseJobID != "" {
		var base *Snapshot
		base, err = db.readSnapshot(plan.BaseJobID)
		if err == nil && base.Revision != plan.BaseRevision {
			err = fmt.Errorf("扫描记录已更新，增量任务未执行")
		}
		if err == nil {
			result, err = expandDirectory(ctx, base, c, plan.IncrementalPath, progress, func(next *Snapshot) error {
				return db.publishDirectory(ctx, id, plan, next, lastProgress, false)
			})
		}
	} else if plan.AnalysisPath != "" {
		result, err = buildDetailSnapshot(ctx, c, plan.AnalysisPath, progress)
	} else {
		result, err = buildSnapshot(ctx, c, progress)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && plan.BaseJobID == "" {
		err = atomicWrite(filepath.Join(db.Directory, "results", id, "snapshot.json"), result)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		lastProgress["phase"], lastProgress["path"] = "completed", "扫描完成"
		if plan.BaseJobID == "" {
			lastProgress["entries"], lastProgress["allocated"], lastProgress["errors"] = result.Scan["visited_entries"], result.Tree.Allocated, result.Tree.Errors
		} else {
			lastProgress["record_allocated"] = result.Tree.Allocated
		}
		lastProgress["current_containers"] = []scanContainer{}
		err = db.publishDirectory(ctx, id, plan, result, lastProgress, true)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		message := err.Error()
		if len(message) > 4000 {
			message = message[:4000]
		}
		if _, updateErr := db.SQL.Exec("UPDATE jobs SET status='failed',finished_at=?,error=? WHERE id=? AND status='running'", platform.Now(), message, id); updateErr != nil {
			return fmt.Errorf("%v; record failure: %w", err, updateErr)
		}
	}
	return err
}
