package cluster

import (
	"net/http"
	"strings"
	"testing"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func TestMemberGPUOverviewAndAccessBoundary(t *testing.T) {
	f := setup(t)
	id, resourceToken := registerResource(t, f, "alice")
	awaitIdle(t, f, id)
	token := statusLogin(t, f, "alice", "Member-password-123")
	path := "/api/status/alice/gpu"
	requireStatus(t, selfCall(f, "GET", path, "", nil), 401)
	requireStatus(t, selfCall(f, "GET", path, resourceToken, nil), 401)
	requireStatus(t, f.request(t, "GET", path, nil), 401)
	requireStatus(t, selfCall(f, "GET", "/api/status/bob/gpu", token, nil), 403)
	requireStatus(t, selfCall(f, "POST", path, token, nil), 404)
	requireStatus(t, selfCall(f, "GET", path+"/overview", token, nil), 404)
	for _, route := range []string{"/api/cluster/overview", "/api/cluster/nodes/" + strings.Repeat("1", 32) + "/api/gpu/overview", "/api/users", "/api/bastion/keys", "/api/gpu/overview"} {
		requireStatus(t, selfCall(f, "GET", route, token, nil), 401)
	}
	overview := map[string]any{
		"now": 1800000000, "sample_seconds": 15,
		"current": map[string]any{"at": 1800000000, "error": "", "warning": "", "devices": []map[string]any{
			{"uuid": "GPU-1", "index": 0, "name": "Test GPU", "utilization": 42, "processes": []map[string]any{{"owner": "bob", "name": "python train.py", "container_id": "bob-container"}}},
		}},
		"history": map[string]any{"from": 1799740800, "to": 1800000000, "step": 300, "series": []map[string]any{
			{"uuid": "GPU-1", "name": "Test GPU", "points": []map[string]any{{"at": 1799999700, "utilization": 42, "observed_seconds": 300, "owners": map[string]int{"bob": 300, "alice": 300}}}},
		}, "users": []map[string]any{{"owner": "bob", "seconds": 300}}},
	}
	calls := 0
	w, s := worker(t, strings.Repeat("1", 32), Inventory{}, moduleFunc(func(_ http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
		calls++
		if r.Method != "GET" || r.URL.RequestURI() != "/api/gpu/overview?end=1800000000&hours=72&step=300" || u.ID != id || u.Role != "viewer" {
			t.Errorf("unexpected GPU request: %s %s %+v", r.Method, r.URL, u)
		}
		return 200, overview, nil
	}))
	add(t, f, w, s, "GPU node")
	query := "?node_id=" + w.ID + "&hours=72&step=300&end=1800000000"
	response := selfCall(f, "GET", path+query, token, nil)
	requireStatus(t, response, 200)
	if strings.TrimSpace(response.Body.String()) != httpapi.JSONText(overview) {
		t.Fatalf("GPU overview was altered: %s", response.Body.String())
	}
	for _, bad := range []string{"", "?node_id=invalid", query + "&path=/api/settings", query + "&hours=1", query + "&url=http://other", query + "&%ZZ=1"} {
		requireStatus(t, selfCall(f, "GET", path+bad, token, nil), 400)
	}
	requireStatus(t, selfCall(f, "GET", path+"?node_id="+strings.Repeat("f", 32), token, nil), 404)
	if calls != 1 {
		t.Fatal("invalid requests reached the worker", calls)
	}
	offline, offlineServer := worker(t, strings.Repeat("2", 32), Inventory{}, nil)
	add(t, f, offline, offlineServer, "Offline")
	offlineServer.Close()
	requireStatus(t, selfCall(f, "GET", path+"?node_id="+offline.ID, token, nil), 502)
	if _, err := f.db.SQL.Exec("UPDATE cluster_nodes SET kind='registry',internal_ip='' WHERE id=?", w.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, selfCall(f, "GET", path+query, token, nil), 400)
	requireStatus(t, selfCall(f, "POST", "/api/status/alice/logout", token, nil), 200)
	requireStatus(t, selfCall(f, "GET", path+query, token, nil), 401)
}
