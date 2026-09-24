package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"project-alpha/internal/httpapi"
)

type hostEvidenceRecords struct {
	Records
	items []object
}

type rejectingHostCleanupRecords struct {
	*testRecords
	called   atomic.Bool
	password []byte
}

func (r *rejectingHostCleanupRecords) DeleteHostReportPaths(ctx context.Context, actor, id string, paths []string, password []byte, result func(string, string, string) error) error {
	r.called.Store(true)
	r.password = password
	return httpapi.NewError(409, "Host 路径归属已变化或无法核实")
}

func (r *hostEvidenceRecords) Query(id, operation string, fields map[string]json.RawMessage) (object, error) {
	if operation == "host_nodes" {
		return object{"items": r.items}, nil
	}
	return nil, fmt.Errorf("unexpected operation %q", operation)
}

func TestHostReportValidatesGroupPathsAndOwnershipEvidence(t *testing.T) {
	group := []reportDirectory{{Path: "/srv", Name: "srv"}}
	valid := `{"directories":[{"path":"/srv","findings":[{"path":"/srv/cache","category":1,"kind":"下载与包缓存","bytes":4096,"summary":"构建缓存","reason":"可重建"}],"note":""}]}`
	parsed, err := parseHostReportGroup(valid, group)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		`{"directories":[]}`,
		`{"directories":[{"path":"/other","findings":[],"note":"无"}]}`,
		strings.Replace(valid, "/srv/cache", "/other/cache", 1),
		strings.Replace(valid, `"bytes":4096`, `"bytes":-1`, 1),
		strings.Replace(valid, `"kind":"下载与包缓存"`, `"kind":"猜测"`, 1),
		valid + ` {}`,
	} {
		if _, err := parseHostReportGroup(invalid, group); err == nil {
			t.Fatalf("accepted invalid Host result: %s", invalid)
		}
	}
	for _, test := range []struct {
		name     string
		path     string
		bytes    int64
		hostOnly bool
		known    bool
		wantErr  bool
	}{
		{"valid", "/srv/cache", 4096, true, true, false},
		{"wrong path", "/srv/other", 4096, true, true, true},
		{"wrong bytes", "/srv/cache", 2048, true, true, true},
		{"mixed parent", "/srv/cache", 4096, false, true, true},
		{"unknown", "/srv/cache", 4096, true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			records := &hostEvidenceRecords{items: []object{{"requested_path": "/srv/cache", "node": object{"path": test.path, "kind": "directory", "allocated": test.bytes, "host_only": test.hostOnly, "known": test.known}}}}
			a := &Manager{records: records}
			err := a.validateHostReportEvidence(context.Background(), "snapshot", parsed)
			if (err != nil) != test.wantErr {
				t.Fatalf("validation error = %v; want error %v", err, test.wantErr)
			}
		})
	}
	// A zero-byte container claim still makes a path unsafe for a Host report.
	zero := hostReportGroupResult{Directories: []hostReportDirectoryResult{{Path: "/srv", Findings: []hostReportFinding{{Path: "/srv/empty", Category: 1, Kind: "其他", Bytes: new(int64), Summary: "空目录", Reason: "可删除"}}}}}
	records := &hostEvidenceRecords{items: []object{{"requested_path": "/srv/empty", "node": object{"path": "/srv/empty", "allocated": int64(0), "host_only": false, "known": true}}}}
	if err := (&Manager{records: records}).validateHostReportEvidence(context.Background(), "snapshot", zero); err == nil {
		t.Fatal("accepted zero-byte container claim")
	}
}

func TestHostReportRetryPublishesExtractableManifestWithoutContainers(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	p.configure()
	job := p.expect(202, "POST", "/api/jobs", object{}, nil)
	snapshotID := job["id"].(string)
	waitJob(t, p.records, snapshotID)
	file := p.storage + "/model.bin"
	nodes, err := p.records.Query(snapshotID, "host_nodes", map[string]json.RawMessage{"paths": json.RawMessage(httpapi.JSONText([]string{file}))})
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Items []struct {
			Node struct {
				Allocated int64 `json:"allocated"`
				HostOnly  bool  `json:"host_only"`
			} `json:"node"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(httpapi.JSONText(nodes)), &evidence); err != nil || len(evidence.Items) != 1 || !evidence.Items[0].Node.HostOnly {
		t.Fatalf("test Host node unavailable: %v %+v", err, evidence)
	}
	bytes := evidence.Items[0].Node.Allocated
	result := hostReportGroupResult{Directories: []hostReportDirectoryResult{{Path: p.storage, Findings: []hostReportFinding{{Path: file, Category: 1, Kind: "下载与包缓存", Bytes: &bytes, Summary: "测试缓存", Reason: "可以重新生成"}}}}}
	var calls atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body object
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["tools"] == nil {
			writeReportReply(w, "completions", httpapi.JSONText(object{"entries": []object{{"path": file, "category": 1, "summary": "测试缓存，可重建"}}}), nil)
			return
		}
		switch calls.Add(1) {
		case 1:
			writeReportReply(w, "completions", "查询 Host 目录", []agentToolCall{{ID: "host-source", Name: "get_host_directory", Arguments: httpapi.JSONText(object{"path": p.storage, "offset": 0, "limit": 30})}})
		case 2:
			http.Error(w, "temporary provider failure", http.StatusBadGateway)
		default:
			if !strings.Contains(httpapi.JSONText(body), "host_allocated") {
				t.Error("retry lost the successful Host directory query")
			}
			writeReportReply(w, "completions", httpapi.JSONText(result), nil)
		}
	}))
	defer mock.Close()
	configureTestAgent(t, p, "completions", mock.URL)
	created := p.expect(202, "POST", "/api/agent/reports", object{"scope": "host", "snapshot_id": snapshotID, "revision": 0, "concurrency": 1}, nil)
	id := created["id"].(string)
	if created["report_scope"] != "host" {
		t.Fatal("Host session scope missing", created)
	}
	first := waitAgentSession(t, p, id)
	if first["session"].(object)["status"] != "failed" {
		t.Fatal("expected first Host agent attempt to fail", first)
	}
	var requestID string
	if err := p.db.SQL.QueryRow(`SELECT json_extract(content,'$.request_id') FROM agent_messages WHERE session_id=? AND role='model_request' AND json_extract(content,'$.group_id')='group-1' ORDER BY id DESC LIMIT 1`, id).Scan(&requestID); err != nil {
		t.Fatal(err)
	}
	p.expect(202, "POST", "/api/agent/sessions/"+id+"/retry", object{"requests": []object{{"group_id": "group-1", "request_id": requestID}}}, nil)
	finished := waitAgentSession(t, p, id)
	if finished["session"].(object)["status"] != "completed" || calls.Load() != 3 {
		t.Fatalf("Host retry failed: %+v calls=%d", finished, calls.Load())
	}
	reports := p.expect(200, "GET", "/api/agent/cleanup-reports", nil, nil)["reports"].([]any)
	if len(reports) != 1 || reports[0].(object)["report_scope"] != "host" {
		t.Fatal("Host report not separated for cleanup", reports)
	}
	reportID := reports[0].(object)["report_id"]
	extract := p.expect(202, "POST", "/api/agent/cleanups", object{"report_id": reportID}, nil)
	cleanupID := extract["id"].(string)
	if extract["report_scope"] != "host" {
		t.Fatal("Host extraction scope missing", extract)
	}
	waitAgentSession(t, p, cleanupID)
	entries := p.expect(200, "GET", "/api/agent/cleanups/"+cleanupID, nil, nil)["entries"].([]any)
	if len(entries) != 1 || entries[0].(object)["path"] != file {
		t.Fatal("Host manifest did not reach extraction", entries)
	}
	// Storage's final check runs under its scan lock. Its rejection must be
	// written to the entry and the transient sudo password must be wiped.
	rejecting := &rejectingHostCleanupRecords{testRecords: p.records}
	p.agent.records = rejecting
	p.expect(202, "POST", "/api/agent/cleanups/"+cleanupID+"/delete", object{"entry_ids": []any{entries[0].(object)["id"]}, "sudo_password": testDeletePassword}, nil)
	waitAgentSession(t, p, cleanupID)
	state := p.expect(200, "GET", "/api/agent/cleanups/"+cleanupID, nil, nil)
	entry := state["entries"].([]any)[0].(object)
	if !rejecting.called.Load() || entry["status"] != "failed" || !strings.Contains(entry["error"].(string), "Host 路径归属") {
		t.Fatal("Host deletion did not dispatch to guarded storage or persist rejection", state)
	}
	for _, b := range rejecting.password {
		if b != 0 {
			t.Fatal("Host sudo password was not wiped")
		}
	}
}

type unavailableHostRecords struct{ Records }

func (r *unavailableHostRecords) Query(id, operation string, fields map[string]json.RawMessage) (object, error) {
	if operation == "host_directory" {
		return nil, httpapi.NewError(400, "目录被排除，无法查询")
	}
	return r.Records.Query(id, operation, fields)
}

func TestHostReportQueryFailureCanFinishWithNote(t *testing.T) {
	for _, scenario := range []string{"note", "unqueried note", "unverified findings", "retry"} {
		t.Run(scenario, func(t *testing.T) {
			p := newTestPlatform(t)
			p.login(true, "administrator", "A-test-password-123")
			p.configure()
			job := p.expect(202, "POST", "/api/jobs", object{}, nil)
			snapshotID := job["id"].(string)
			waitJob(t, p.records, snapshotID)
			p.agent.records = &unavailableHostRecords{Records: p.records}
			note := httpapi.JSONText(hostReportGroupResult{Directories: []hostReportDirectoryResult{{Path: p.storage, Findings: []hostReportFinding{}, Note: "目录被排除，无法核实容量和用途"}}})
			file := p.storage + "/model.bin"
			nodes, err := p.records.Query(snapshotID, "host_nodes", map[string]json.RawMessage{"paths": json.RawMessage(httpapi.JSONText([]string{file}))})
			if err != nil {
				t.Fatal(err)
			}
			var evidence struct {
				Items []struct {
					Node struct {
						Allocated int64 `json:"allocated"`
					} `json:"node"`
				} `json:"items"`
			}
			if err := json.Unmarshal([]byte(httpapi.JSONText(nodes)), &evidence); err != nil {
				t.Fatal(err)
			}
			bytes := evidence.Items[0].Node.Allocated
			finding := httpapi.JSONText(hostReportGroupResult{Directories: []hostReportDirectoryResult{{Path: p.storage, Findings: []hostReportFinding{{Path: file, Category: 1, Kind: "其他", Bytes: &bytes, Summary: "未核实文件", Reason: "猜测可删除"}}}}})
			var calls atomic.Int32
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				if scenario == "unqueried note" && call == 1 {
					writeReportReply(w, "completions", note, nil)
					return
				}
				queryCall := int32(1)
				if scenario == "unqueried note" {
					queryCall = 2
				}
				if call == queryCall {
					writeReportReply(w, "completions", "核实目录", []agentToolCall{{ID: "host-source", Name: "get_host_directory", Arguments: httpapi.JSONText(object{"path": p.storage, "offset": 0, "limit": 30})}})
					return
				}
				if call == 2 && scenario == "unverified findings" {
					writeReportReply(w, "completions", finding, nil)
					return
				}
				if call == 2 && scenario == "retry" {
					http.Error(w, "provider failure", 502)
					return
				}
				if call > 3 {
					http.Error(w, "unexpected correction loop", 500)
					return
				}
				writeReportReply(w, "completions", note, nil)
			}))
			defer mock.Close()
			configureTestAgent(t, p, "completions", mock.URL)
			created := p.expect(202, "POST", "/api/agent/reports", object{"scope": "host", "snapshot_id": snapshotID, "revision": 0, "concurrency": 1}, nil)
			id := created["id"].(string)
			finished := waitAgentSession(t, p, id)
			if scenario == "retry" {
				if finished["session"].(object)["status"] != "failed" {
					t.Fatal("expected provider failure", finished)
				}
				var requestID string
				if err := p.db.SQL.QueryRow(`SELECT json_extract(content,'$.request_id') FROM agent_messages WHERE session_id=? AND role='model_request' ORDER BY id DESC LIMIT 1`, id).Scan(&requestID); err != nil {
					t.Fatal(err)
				}
				p.expect(202, "POST", "/api/agent/sessions/"+id+"/retry", object{"requests": []object{{"group_id": "group-1", "request_id": requestID}}}, nil)
				finished = waitAgentSession(t, p, id)
			}
			wantCalls := int32(3)
			if scenario == "note" {
				wantCalls = 2
			}
			if finished["session"].(object)["status"] != "completed" || calls.Load() != wantCalls {
				t.Fatalf("query failure did not finish correctly: calls=%d session=%v", calls.Load(), finished)
			}
			var manifest string
			if err := p.db.SQL.QueryRow(`SELECT r.entries FROM agent_reports r JOIN agent_messages m ON m.id=r.message_id WHERE m.session_id=?`, id).Scan(&manifest); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(manifest, file) {
				t.Fatal("unverified finding published", manifest)
			}
		})
	}
}

func TestHostReportPublishesDirectoryWithFoldedHostHardLinks(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	p.configure()
	other := filepath.Join(p.storage, "folded")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(other, "original"), make([]byte, 4096))
	if err := os.Link(filepath.Join(other, "original"), filepath.Join(other, "alias")); err != nil {
		t.Fatal(err)
	}
	job := p.expect(202, "POST", "/api/jobs", object{}, nil)
	snapshotID := job["id"].(string)
	waitJob(t, p.records, snapshotID)
	overview, err := p.records.Query(snapshotID, "overview", nil)
	if err != nil || overview["attribution_limited"] != true {
		t.Fatalf("fixture needs folded references: %v %v", overview, err)
	}
	// Publish a measured directory containing folded Host-to-Host hard links.
	file := other
	nodes, err := p.records.Query(snapshotID, "host_nodes", map[string]json.RawMessage{"paths": json.RawMessage(httpapi.JSONText([]string{file}))})
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Items []struct {
			Node struct {
				Allocated int64
				HostOnly  bool `json:"host_only"`
			}
		}
	}
	if err := json.Unmarshal([]byte(httpapi.JSONText(nodes)), &evidence); err != nil {
		t.Fatal(err)
	}
	if len(evidence.Items) != 1 || !evidence.Items[0].Node.HostOnly {
		t.Fatalf("folded Host directory was not certified: %v", nodes)
	}
	bytes := evidence.Items[0].Node.Allocated
	result := hostReportGroupResult{Directories: []hostReportDirectoryResult{{Path: p.storage, Findings: []hostReportFinding{{Path: file, Category: 2, Kind: "模型权重", Bytes: &bytes, Summary: "模型文件", Reason: "核对使用需求后再处理"}}}}}
	var calls atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			writeReportReply(w, "completions", "核对 Host 证据", []agentToolCall{{ID: "host-source", Name: "get_host_directory", Arguments: httpapi.JSONText(object{"path": p.storage, "offset": 0, "limit": 30})}})
		case 2:
			writeReportReply(w, "completions", httpapi.JSONText(result), nil)
		default:
			http.Error(w, "unexpected evidence correction", 500)
		}
	}))
	defer mock.Close()
	configureTestAgent(t, p, "completions", mock.URL)
	created := p.expect(202, "POST", "/api/agent/reports", object{"scope": "host", "snapshot_id": snapshotID, "revision": 0, "concurrency": 1}, nil)
	id := created["id"].(string)
	finished := waitAgentSession(t, p, id)
	if finished["session"].(object)["status"] != "completed" || calls.Load() != 2 {
		t.Fatalf("local Host evidence rejected: %v", finished)
	}
	var manifest string
	if err := p.db.SQL.QueryRow(`SELECT r.entries FROM agent_reports r JOIN agent_messages m ON m.id=r.message_id WHERE m.session_id=?`, id).Scan(&manifest); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(manifest, file) {
		t.Fatalf("Host report is empty: %s", manifest)
	}
}
