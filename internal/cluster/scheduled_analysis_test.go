package cluster

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
	"project-alpha/internal/storage"
)

func TestScheduledAnalysisQueueReconnectAndAuthorization(t *testing.T) {
	f := setup(t)
	// Drive the consumer explicitly so the test can drop exactly one acknowledgement.
	f.control.analysisCancel()
	<-f.control.analysisDone
	id, record := strings.Repeat("a", 32), strings.Repeat("b", 32)
	db, err := platform.OpenDatabase(t.TempDir(), storage.Initialize)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	handler := storage.NewHandler(storage.NewStore(db), &storage.Manager{})
	acknowledgements := 0
	module := moduleFunc(func(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
		if r.Method == "POST" && r.URL.Path == "/api/worker/scheduled-analysis" {
			acknowledgements++
			if acknowledgements == 1 {
				return 0, nil, httpapi.NewError(503, "lost acknowledgement")
			}
		}
		return handler.Dispatch(w, r, user)
	})
	worker, server := worker(t, id, Inventory{}, module)
	add(t, f, worker, server, "scheduled")
	node, err := f.control.node(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec("INSERT INTO jobs(id,status,trigger,created_by,created_at,config,analysis_status,analysis_user_id) VALUES(?,'completed','scheduled','scheduler',1,'{}','pending',?)", record, f.user.ID); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"host", "container"} {
		if _, err := f.db.SQL.Exec("INSERT INTO agent_sessions(id,user_id,node_id,title,status,created_at,updated_at,provider,model,report_scope,scheduled_job_id,snapshot_id) VALUES(?,?,?,'scheduled','completed',1,2,'completions','test',?,?,?)", scope, f.user.ID, node.ID, scope, record, record); err != nil {
			t.Fatal(err)
		}
		r, err := f.db.SQL.Exec("INSERT INTO agent_messages(session_id,role,content,created_at) VALUES(?,'assistant','report',2)", scope)
		if err != nil {
			t.Fatal(err)
		}
		messageID, _ := r.LastInsertId()
		if _, err := f.db.SQL.Exec("INSERT INTO agent_reports VALUES(?,'[]')", messageID); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.control.consumeScheduledAnalysis(context.Background(), node); err == nil {
		t.Fatal("lost acknowledgement was ignored")
	}
	for range 2 {
		if err := f.control.consumeScheduledAnalysis(context.Background(), node); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.db.SQL.QueryRow("SELECT count(*) FROM agent_cleanups").Scan(&count); err != nil || count != 2 {
		t.Fatalf("cleanup count=%d: %v", count, err)
	}
	var status string
	if err := db.SQL.QueryRow("SELECT analysis_status FROM jobs WHERE id=?", record).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("status=%s: %v", status, err)
	}
	// Internal acknowledgements cannot be reached through the browser proxy.
	response := f.request(t, "POST", "/api/cluster/nodes/"+node.ID+"/api/worker/scheduled-analysis", map[string]any{})
	requireStatus(t, response, http.StatusNotFound)
	if _, err := db.SQL.Exec("UPDATE jobs SET analysis_status='pending',analysis_user_id='revoked-admin'"); err != nil {
		t.Fatal(err)
	}
	if err := f.control.consumeScheduledAnalysis(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	var message string
	if err := db.SQL.QueryRow("SELECT analysis_status,analysis_error FROM jobs").Scan(&status, &message); err != nil || status != "failed" || !strings.Contains(message, "管理员权限") {
		t.Fatalf("revocation: %s %s %v", status, message, err)
	}
	viewerRequest := httptest.NewRequest("GET", "/api/worker/scheduled-analysis", nil)
	if _, _, err := handler.Dispatch(httptest.NewRecorder(), viewerRequest, platform.User{Role: "viewer"}); err == nil {
		t.Fatal("viewer read queue")
	}
}
