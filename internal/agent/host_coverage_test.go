package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"project-alpha/internal/httpapi"
)

type hostCoverageRecords struct {
	Records
	root  string
	scans atomic.Int32
}

func (r *hostCoverageRecords) Query(id, operation string, fields map[string]json.RawMessage) (object, error) {
	if operation != "host_coverage" {
		return r.Records.Query(id, operation, fields)
	}
	return object{"revision": 0, "host_allocated": int64(512 << 20), "covered_allocated": int64(0), "docker_allocated": int64(0), "remaining_allocated": int64(512 << 20), "remaining_count": 1, "remaining": []object{{"path": r.root, "host_allocated": int64(512 << 20), "kind": "folded", "next_tool": "scan_directory", "depth": 1}}}, nil
}
func (r *hostCoverageRecords) Explore(actor, id, path string, revision int64, depth int) (object, error) {
	r.scans.Add(1)
	return r.Records.Explore(actor, id, path, revision, depth)
}

func TestHostReportReviewsCoverageOnceAndResumesWithoutRepeatedScan(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "remaining gap", true: "provider retry"}[retry], func(t *testing.T) {
			p := newTestPlatform(t)
			p.Login(true, "administrator", "A-test-password-123")
			p.configure()
			job := p.Expect(202, "POST", "/api/jobs", object{}, nil)
			snapshotID := job["id"].(string)
			waitJob(t, p.records, snapshotID)
			records := &hostCoverageRecords{Records: p.records, root: p.storage}
			p.agent.records = records
			result := httpapi.JSONText(hostReportGroupResult{Directories: []hostReportDirectoryResult{{Path: p.storage, Findings: []hostReportFinding{}, Note: "已检查目录；剩余用途仍待确认"}}})
			var calls atomic.Int32
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body object
				_ = json.NewDecoder(r.Body).Decode(&body)
				switch call := calls.Add(1); call {
				case 1:
					writeReportReply(w, "completions", "", []agentToolCall{{ID: "root", Name: "get_host_directory", Arguments: httpapi.JSONText(object{"path": p.storage, "offset": 0, "limit": 30})}})
				case 2:
					writeReportReply(w, "completions", result, nil)
				case 3:
					if !strings.Contains(httpapi.JSONText(body), "Host 容量对账仍有大额未覆盖项") {
						t.Error("missing automatic coverage feedback")
					}
					writeReportReply(w, "completions", "", []agentToolCall{{ID: "expand", Name: "scan_directory", Arguments: httpapi.JSONText(object{"path": p.storage, "depth": 1})}})
				case 4:
					if retry {
						http.Error(w, "temporary provider failure", 502)
						return
					}
					writeReportReply(w, "completions", result, nil)
				case 5:
					if !retry {
						t.Error("coverage caused another review")
					}
					writeReportReply(w, "completions", result, nil)
				default:
					http.Error(w, "unexpected loop", 500)
				}
			}))
			defer mock.Close()
			configureTestAgent(t, p, "completions", mock.URL)
			created := p.Expect(202, "POST", "/api/agent/reports", object{"scope": "host", "snapshot_id": snapshotID, "revision": 0, "concurrency": 1}, nil)
			id := created["id"].(string)
			finished := waitAgentSession(t, p, id)
			if retry {
				if finished["session"].(object)["status"] != "failed" {
					t.Fatal("provider did not fail")
				}
				var requestID string
				if err := p.db.SQL.QueryRow(`SELECT json_extract(content,'$.request_id') FROM agent_messages WHERE session_id=? AND role='model_request' ORDER BY id DESC LIMIT 1`, id).Scan(&requestID); err != nil {
					t.Fatal(err)
				}
				p.Expect(202, "POST", "/api/agent/sessions/"+id+"/retry", object{"requests": []object{{"group_id": "group-1", "request_id": requestID}}}, nil)
				finished = waitAgentSession(t, p, id)
			}
			if finished["session"].(object)["status"] != "completed" || records.scans.Load() != 1 {
				t.Fatalf("review failed or repeated scan: %v scans=%d", finished, records.scans.Load())
			}
			var count int
			var report string
			if err := p.db.SQL.QueryRow("SELECT count(*) FROM agent_messages WHERE session_id=? AND role='host_coverage_review'", id).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if err := p.db.SQL.QueryRow("SELECT content FROM agent_messages WHERE session_id=? AND role='assistant' ORDER BY id DESC LIMIT 1", id).Scan(&report); err != nil {
				t.Fatal(err)
			}
			if count != 1 || !strings.Contains(report, "Host 容量对账") || !strings.Contains(report, "512.0 MiB") || !strings.Contains(report, "尚未展开") {
				t.Fatalf("remaining gap hidden or review repeated: %s", report)
			}
		})
	}
}
