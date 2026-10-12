package rootless

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
	"project-alpha/internal/containerorigin"
)

type originStore struct{ directory string }

func (s originStore) read(id string) (containerorigin.Record, error) {
	var record containerorigin.Record
	if !containerorigin.ValidID(id) {
		return record, errors.New("容器 ID 无效")
	}
	f, err := os.OpenFile(filepath.Join(s.directory, id+".json"), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return record, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return record, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return record, errors.New("创建来源记录类型或长度无效；原文件已保留")
	}
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&record); err != nil {
		return record, fmt.Errorf("创建来源记录无效；原文件已保留: %w", err)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return record, errors.New("创建来源记录含多余内容；原文件已保留")
	}
	if record.Version != containerorigin.Version {
		return record, errors.New("创建来源格式不匹配，请使用新的审计目录；原文件已保留")
	}
	_, timeErr := time.Parse(time.RFC3339Nano, record.CreatedAt)
	if record.ContainerID != id || record.BootID == "" || timeErr != nil || (record.Source.ContainerID != "" && !containerorigin.ValidID(record.Source.ContainerID)) || (record.Source.Error == "" && (record.Source.PID <= 0 || record.Source.StartTicks == 0)) {
		return record, errors.New("创建来源字段无效；原文件已保留")
	}
	return record, nil
}

// Publish each immutable record atomically without replacing an earlier one.
func (s originStore) save(record containerorigin.Record) error {
	if !containerorigin.ValidID(record.ContainerID) {
		return errors.New("创建返回的容器 ID 无效")
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.directory, ".origin-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(raw, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Link(f.Name(), filepath.Join(s.directory, record.ContainerID+".json")); err != nil {
		return err
	}
	dir, err := os.Open(s.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s originStore) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		http.Error(w, "需要 GET", http.StatusMethodNotAllowed)
		return
	}
	ids := r.URL.Query()["container_id"]
	if len(ids) == 0 || len(ids) > 128 {
		http.Error(w, "每次查询需要 1–128 个容器 ID", 400)
		return
	}
	response := containerorigin.Response{Version: containerorigin.Version, Origins: []containerorigin.Record{}}
	seen := map[string]bool{}
	for _, id := range ids {
		if !containerorigin.ValidID(id) {
			http.Error(w, "容器 ID 无效", 400)
			return
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		record, err := s.read(id)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		response.Origins = append(response.Origins, record)
	}
	// Encode fully before writing so a storage error never produces partial JSON.
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(response); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_, _ = w.Write(body.Bytes())
}
