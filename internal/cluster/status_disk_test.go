package cluster

import (
	"net/http"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

func TestMemberDiskProxyAccessBoundary(t *testing.T) {
	f := setup(t)
	id, resourceToken := registerResource(t, f, "alice")
	awaitIdle(t, f, id)
	token := statusLogin(t, f, "alice", "Member-password-123")
	path := "/api/status/alice/disk"
	for _, credential := range []string{"", resourceToken} {
		requireStatus(t, selfCall(f, "GET", path, credential, nil), 401)
	}
	requireStatus(t, selfCall(f, "GET", "/api/status/bob/disk", token, nil), 403)
	requireStatus(t, selfCall(f, "POST", path, token, nil), 404)
	requireStatus(t, selfCall(f, "GET", path+"/extra", token, nil), 404)
	for _, route := range []string{"/api/member-disk", "/api/snapshot", "/api/jobs", "/api/cluster/nodes/" + strings.Repeat("1", 32) + "/api/member-disk"} {
		requireStatus(t, selfCall(f, "GET", route, token, nil), 401)
	}
	calls := 0
	w, server := worker(t, strings.Repeat("1", 32), Inventory{}, moduleFunc(func(_ http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
		calls++
		if r.Method != "GET" || r.URL.RequestURI() != "/api/member-disk?container=mine&offset=50&path=%2Fdata" || user.ID != id || user.Username != "member:alice" || user.Role != "viewer" {
			t.Errorf("unexpected member disk request: %s %+v", r.URL, user)
		}
		return 200, map[string]any{"entries": []any{}}, nil
	}))
	add(t, f, w, server, "Disk node")
	query := "?node_id=" + w.ID + "&container=mine&path=%2Fdata&offset=50"
	requireStatus(t, selfCall(f, "GET", path+query, token, nil), 200)
	for _, bad := range []string{"", "?node_id=bad", query + "&node_id=" + w.ID, query + "&owner=bob", query + "&url=http://other", query + "&%ZZ=1"} {
		requireStatus(t, selfCall(f, "GET", path+bad, token, nil), 400)
	}
	requireStatus(t, selfCall(f, "GET", path+"?node_id="+strings.Repeat("f", 32), token, nil), 404)
	if calls != 1 {
		t.Fatal("invalid requests reached worker", calls)
	}
	server.Close()
	requireStatus(t, selfCall(f, "GET", path+query, token, nil), 502)
	requireStatus(t, selfCall(f, "POST", "/api/status/alice/logout", token, nil), 200)
	requireStatus(t, selfCall(f, "GET", path+query, token, nil), 401)
}
