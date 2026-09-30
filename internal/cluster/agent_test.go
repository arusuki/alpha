package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func TestControlRecoveryOwnsDirectory(t *testing.T) {
	f := setup(t)
	node, server := worker(t, strings.Repeat("a", 32), Inventory{}, nil)
	add(t, f, node, server, "worker")
	_, err := f.db.SQL.Exec("INSERT INTO agent_sessions(id,node_id,user_id,title,status,created_at,updated_at,active_job_id,provider,model) VALUES('session',?,?,'test','running',0,0,'job','completions','test')", node.ID, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Opening a manager must not repeat service recovery.
	requireStatus(t, f.request(t, "GET", "/api/cluster/nodes/"+node.ID+"/api/agent/sessions", nil), 200)
	if other, err := NewControl(f.db); err == nil {
		other.Close()
		t.Fatal("two controls acquired the same data directory")
	}
	var status string
	if err := f.db.SQL.QueryRow("SELECT status FROM agent_sessions WHERE id='session'").Scan(&status); err != nil || status != "running" {
		t.Fatalf("live session was recovered: %s %v", status, err)
	}
	f.control.Close()
	// Recovery includes conversations belonging to removed nodes.
	if _, err := f.db.SQL.Exec("DELETE FROM cluster_nodes"); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewControl(f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	var count int
	if err := f.db.SQL.QueryRow("SELECT count(*) FROM agent_sessions WHERE status='interrupted' AND active_job_id IS NULL AND updated_at>0").Scan(&count); err != nil || count != 1 {
		t.Fatalf("restart did not recover orphaned work: %d %v", count, err)
	}
}

func TestWorkerUnauthorizedDoesNotInvalidateControlSession(t *testing.T) {
	f := setup(t)
	node, server := worker(t, strings.Repeat("a", 32), Inventory{}, moduleFunc(func(http.ResponseWriter, *http.Request, platform.User) (int, any, error) {
		return 0, nil, httpapi.NewError(401, "节点令牌已失效")
	}))
	add(t, f, node, server, "worker")
	requireStatus(t, f.request(t, "GET", "/api/cluster/nodes/"+node.ID+"/api/state", nil), 502)
	requireStatus(t, f.request(t, "GET", "/api/session", nil), 200)
	if session, err := f.db.Session(f.token); err != nil || session.User.ID != f.user.ID {
		t.Fatalf("session lost: %v", err)
	}
}

func TestCentralAgentSettingsAndRecordDeletion(t *testing.T) {
	f := setup(t)
	record := strings.Repeat("c", 32)
	node, server := worker(t, strings.Repeat("a", 32), Inventory{}, moduleFunc(func(_ http.ResponseWriter, r *http.Request, _ platform.User) (int, any, error) {
		if r.Method != "DELETE" || r.URL.Path != "/api/jobs/"+record {
			t.Errorf("unexpected worker request: %s %s", r.Method, r.URL.Path)
		}
		return 200, map[string]any{"deleted_ids": []string{record}, "cleanup_pending": false}, nil
	}))
	add(t, f, node, server, "worker")
	requireStatus(t, f.request(t, "PUT", "/api/agent/settings", map[string]any{"revision": 1, "value": map[string]any{"model": "central-model", "api_key": "central-secret"}}), 200)
	settings := f.request(t, "GET", "/api/agent/settings", nil)
	requireStatus(t, settings, 200)
	if !strings.Contains(settings.Body.String(), "central-model") || strings.Contains(settings.Body.String(), "central-secret") {
		t.Fatal(settings.Body.String())
	}
	requireStatus(t, f.request(t, "GET", "/api/cluster/nodes/"+node.ID+"/api/agent/settings", nil), 404)
	session := strings.Repeat("d", 32)
	_, err := f.db.SQL.Exec("INSERT INTO agent_sessions(id,node_id,user_id,title,status,created_at,updated_at,snapshot_id,provider,model) VALUES(?,?,?,'test','running',0,0,?,'completions','test')", session, node.ID, f.user.ID, record)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/cluster/nodes/" + node.ID + "/api/jobs/" + record
	requireStatus(t, f.request(t, "DELETE", path, nil), 409)
	if _, err = f.db.SQL.Exec("UPDATE agent_sessions SET status='completed' WHERE id=?", session); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f.request(t, "DELETE", path, nil), 200)
	var count int
	if err = f.db.SQL.QueryRow("SELECT count(*) FROM agent_sessions WHERE id=? AND snapshot_id IS NULL", session).Scan(&count); err != nil || count != 1 {
		t.Fatalf("dangling record: %d %v", count, err)
	}
}

func TestRemoteToolsIdentityCleanupResultsAndDisconnect(t *testing.T) {
	f := setup(t)
	node, server := worker(t, strings.Repeat("a", 32), Inventory{}, nil)
	node.Tools = func(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
		if user != f.user {
			t.Errorf("tool identity = %+v", user)
		}
		var args map[string]any
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			t.Fatal(err)
		}
		if args["operation"] == "query" {
			return 200, map[string]any{"revision": 7, "node": node.ID, "allocated": json.Number("9007199254740993")}, nil
		}
		if args["operation"] != "cleanup" || args["password"] != "test-password" || args["host"] != true {
			t.Errorf("invalid cleanup: %v", args)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		encoder := json.NewEncoder(w)
		encoder.Encode(map[string]any{"path": "/srv/cache", "status": "deleted", "message": ""})
		if args["id"] == strings.Repeat("b", 32) {
			encoder.Encode(map[string]any{"done": true})
		}
		return 0, nil, nil
	}
	add(t, f, node, server, "worker")
	remote := &remoteRecords{control: f.control, nodeID: node.ID, userID: f.user.ID}
	result, err := remote.Query(strings.Repeat("b", 32), "overview", nil)
	if err != nil || result["node"] != node.ID || result["allocated"] != json.Number("9007199254740993") {
		t.Fatalf("query=%v %v", result, err)
	}
	for _, id := range []string{strings.Repeat("b", 32), strings.Repeat("c", 32)} {
		password := []byte("test-password")
		var paths []string
		err := remote.DeleteHostReportPaths(context.Background(), f.user.Username, id, []string{"/srv/cache"}, password, func(path, status, message string) error { paths = append(paths, path); return nil })
		if len(paths) != 1 || paths[0] != "/srv/cache" {
			t.Fatalf("lost cleanup confirmation: %v", paths)
		}
		if id == strings.Repeat("b", 32) && err != nil {
			t.Fatal(err)
		}
		if id == strings.Repeat("c", 32) {
			var apiErr *httpapi.Error
			if err == nil || errors.As(err, &apiErr) {
				t.Fatalf("interrupted deletion must remain uncertain: %v", err)
			}
		}
		for _, b := range password {
			if b != 0 {
				t.Fatal("cleanup password retained")
			}
		}
	}
	if _, err = f.db.SQL.Exec("UPDATE users SET role='viewer' WHERE id=?", f.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = remote.Query(strings.Repeat("b", 32), "overview", nil); err == nil {
		t.Fatal("revoked user called node tool")
	}
}
