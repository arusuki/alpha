package storage

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Hash the public representation, including live owner overrides and directory
// revisions. The validator and response body always describe the same read.
func (s *Handler) serveSnapshot(w http.ResponseWriter, r *http.Request, id string) (int, any, error) {
	snapshot, err := s.Snapshot(id)
	if err != nil {
		return 0, nil, err
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		return 0, nil, err
	}
	etag := fmt.Sprintf(`"%x"`, sha256.Sum256(body))
	w.Header().Set("ETag", etag)
	for _, candidate := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			w.WriteHeader(http.StatusNotModified)
			return 0, nil, nil
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return 0, nil, nil
}
