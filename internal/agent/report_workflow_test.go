package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"project-alpha/internal/httpapi"
)

// Synthetic Docker data with real session persistence and provider/tool loops.
type reportFixture struct {
	Records
	containers []reportContainer
	revision   atomic.Int64
	scans      atomic.Int64
	reads      atomic.Int64
}

func (r *reportFixture) Query(id, operation string, fields map[string]json.RawMessage) (object, error) {
	result := object{"scope": "container", "snapshot_id": id, "revision": r.revision.Load(), "updated_at": "2026-09-15", "observed_at": "2026-09-14"}
	switch operation {
	case "overview":
		result["containers"] = len(r.containers)
	case "containers":
		var offset, limit int
		_ = json.Unmarshal(fields["offset"], &offset)
		_ = json.Unmarshal(fields["limit"], &limit)
		if limit == 0 {
			limit = 30
		}
		result["items"] = r.containers[min(offset, len(r.containers)):min(offset+limit, len(r.containers))]
		result["total"] = len(r.containers)
		result["has_more"] = offset+limit < len(r.containers)
	case "container":
		var container string
		_ = json.Unmarshal(fields["container"], &container)
		r.reads.Add(1)
		result["usage"] = object{"container": object{"id": container, "upper_path": "/private/" + container, "mounts": []object{{"source": "/shared/models", "destination": "/models"}}}}
		result["sources"] = []object{{"path": "/shared/models", "node": object{"allocated": 8192}}}
	case "nodes":
		var paths []string
		_ = json.Unmarshal(fields["paths"], &paths)
		items := []object{}
		for _, p := range paths {
			entry := object{"requested_path": p}
			if p == "/shared/models" {
				entry["node"] = object{"path": p, "allocated": 8192, "known": true}
			}
			items = append(items, entry)
		}
		result["items"] = items
	case "directory":
		result["node"] = object{"path": "/shared/models", "allocated": 8192, "known": true}
	default:
		return nil, fmt.Errorf("unexpected fixture query %q", operation)
	}
	return result, nil
}

func (r *reportFixture) Explore(actor, id, path string, revision int64, depth int) (object, error) {
	if revision != r.revision.Load() {
		return nil, fmt.Errorf("stale fixture revision")
	}
	r.scans.Add(1)
	r.revision.Add(1)
	return object{"id": fmt.Sprintf("detail-%d", r.scans.Load())}, nil
}
func (r *reportFixture) Wait(ctx context.Context, id string, check func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return check()
}
func (r *reportFixture) Cancel(id, actor string) (object, error) { return object{}, nil }

func (r *reportFixture) Job(id string) (object, error) {
	if strings.HasPrefix(id, "detail-") {
		return object{"id": id, "status": "completed", "trigger": "incremental"}, nil
	}
	return r.Records.Job(id)
}

func fixtureContainers(count int) []reportContainer {
	result := []reportContainer{}
	for i := 0; i < count; i++ {
		result = append(result, reportContainer{ID: fmt.Sprintf("container-%02d", i), Name: fmt.Sprintf("worker-%02d", i)})
	}
	return result
}

func fixtureReport(group []reportContainer) reportGroupResult {
	result := reportGroupResult{}
	for _, c := range group {
		bytes := int64(8192)
		result.Containers = append(result.Containers, reportContainerResult{ContainerID: c.ID, Findings: []reportFinding{{Path: "/shared/models", ContainerPath: "/models", Category: 2, Kind: "模型权重", Bytes: &bytes, Summary: "共享的模型权重。", Reason: "尚未确定任务依赖，不能直接删除。"}}})
	}
	return result
}

func writeReportReply(w http.ResponseWriter, protocol, answer string, calls []agentToolCall) {
	if protocol == "completions" {
		message := object{"role": "assistant", "content": answer}
		if len(calls) > 0 {
			items := []object{}
			for _, c := range calls {
				items = append(items, object{"id": c.ID, "type": "function", "function": object{"name": c.Name, "arguments": c.Arguments}})
			}
			message["tool_calls"] = items
		}
		writeModelReply(w, object{"choices": []object{{"message": message}}})
	} else {
		output := []object{}
		for _, c := range calls {
			output = append(output, object{"type": "function_call", "call_id": c.ID, "name": c.Name, "arguments": c.Arguments})
		}
		if answer != "" {
			output = append(output, object{"type": "message", "role": "assistant", "content": []object{{"type": "output_text", "text": answer}}})
		}
		writeModelReply(w, object{"status": "completed", "output": output})
	}
}

func TestDiskReportGroupsExploreIndependentlyAndSupportFollowup(t *testing.T) {
	for _, protocol := range []string{"completions", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			p := newTestPlatform(t)
			p.login(true, "administrator", "A-test-password-123")
			p.configure()
			selected := p.expect(202, "POST", "/api/jobs", object{}, nil)
			id := selected["id"].(string)
			waitJob(t, p.records, id)
			latest := p.expect(202, "POST", "/api/jobs", object{}, nil)
			waitJob(t, p.records, latest["id"].(string))
			p.records.failStart = true
			fixture := &reportFixture{Records: p.records, containers: fixtureContainers(9)}
			p.agent.records = fixture
			var calls atomic.Int32
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body object
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				encoded := httpapi.JSONText(body)
				n := int(calls.Add(1)) - 1
				if strings.Contains(encoded, latest["id"].(string)) || !strings.Contains(encoded, id) {
					t.Error("request did not use selected record")
				}
				if n == 9 {
					if !strings.Contains(encoded, "继续查看缓存") || !strings.Contains(encoded, "9 个容器 · 3 组") {
						t.Error("followup missing final report or question")
					}
					if strings.Contains(encoded, "call-g0") {
						t.Error("followup replayed previous tools")
					}
					writeReportReply(w, protocol, "已核对缓存。", nil)
					return
				}
				if n > 9 {
					t.Error("unexpected extra model request")
					writeReportReply(w, protocol, "unexpected", nil)
					return
				}
				groupIndex, step := n/3, n%3
				start := groupIndex * 4
				group := fixture.containers[start:min(start+4, 9)]
				for i, c := range fixture.containers {
					if strings.Contains(encoded, c.ID) != (i >= start && i < start+len(group)) {
						t.Errorf("group %d has wrong context for %s", groupIndex, c.ID)
					}
				}
				if groupIndex > 0 && strings.Contains(encoded, fmt.Sprintf("call-g%d", groupIndex-1)) {
					t.Error("groups shared tool history")
				}
				switch step {
				case 0:
					var toolCalls []agentToolCall
					for i, c := range group {
						toolCalls = append(toolCalls, agentToolCall{ID: fmt.Sprintf("call-g%d-c%d", groupIndex, i), Name: "get_container", Arguments: httpapi.JSONText(object{"container": c.ID})})
					}
					writeReportReply(w, protocol, "核对本组存储来源。", toolCalls)
				case 1:
					var toolCalls []agentToolCall
					for i := 0; i < 8; i++ {
						toolCalls = append(toolCalls, agentToolCall{ID: fmt.Sprintf("call-g%d-s%d", groupIndex, i), Name: "scan_directory", Arguments: httpapi.JSONText(object{"path": "/shared/models", "depth": 3})})
					}
					writeReportReply(w, protocol, "补查主要模型目录。", toolCalls)
				case 2:
					if strings.Contains(encoded, `\"error\"`) {
						t.Error("batched scans failed")
					}
					writeReportReply(w, protocol, httpapi.JSONText(fixtureReport(group)), nil)
				}
			}))
			defer mock.Close()
			configureTestAgent(t, p, protocol, mock.URL)
			created := p.expect(202, "POST", "/api/agent/reports", object{"scope": "container", "snapshot_id": id, "revision": 0, "concurrency": 1}, nil)
			sessionID := created["id"].(string)
			result := waitAgentSession(t, p, sessionID)
			if result["session"].(object)["status"] != "completed" || created["snapshot_id"] != id {
				t.Fatalf("bad report: %v", result)
			}
			if calls.Load() != 9 || fixture.scans.Load() != 24 || fixture.reads.Load() != 18 {
				t.Fatalf("group execution counts: models=%d scans=%d containers=%d", calls.Load(), fixture.scans.Load(), fixture.reads.Load())
			}
			var groupCount, finalCount int
			p.db.SQL.QueryRow("SELECT count(*) FROM agent_messages WHERE session_id=? AND role='group_report'", sessionID).Scan(&groupCount)
			p.db.SQL.QueryRow("SELECT count(*) FROM agent_messages WHERE session_id=? AND role='assistant'", sessionID).Scan(&finalCount)
			var final string
			p.db.SQL.QueryRow("SELECT content FROM agent_messages WHERE session_id=? AND role='assistant'", sessionID).Scan(&final)
			if groupCount != 3 || finalCount != 1 || strings.Count(final, "| /shared/models |") != 1 || !strings.Contains(final, "版本 24") {
				t.Fatalf("bad merged report: %d groups %d finals %s", groupCount, finalCount, final)
			}
			rows, err := p.db.SQL.Query("SELECT role,content FROM agent_messages WHERE session_id=? ORDER BY id", sessionID)
			if err != nil {
				t.Fatal(err)
			}
			active, completed, planned := "", 0, false
			for rows.Next() {
				var role, content string
				if err := rows.Scan(&role, &content); err != nil {
					t.Fatal(err)
				}
				switch role {
				case "report_plan":
					var plan struct {
						Groups []struct {
							ID         string            `json:"id"`
							Number     int               `json:"number"`
							Containers []reportContainer `json:"containers"`
						} `json:"groups"`
					}
					if err := json.Unmarshal([]byte(content), &plan); err != nil || len(plan.Groups) != 3 {
						t.Fatalf("bad sidebar plan: %s, %v", content, err)
					}
					for i, group := range plan.Groups {
						want := fixture.containers[i*4 : min(i*4+4, 9)]
						if group.ID != fmt.Sprintf("group-%d", i+1) || group.Number != i+1 || httpapi.JSONText(group.Containers) != httpapi.JSONText(want) {
							t.Fatalf("incorrect agent assignment: %+v", group)
						}
					}
					planned = true
				case "group_state":
					var state struct{ ID, Status string }
					if err := json.Unmarshal([]byte(content), &state); err != nil {
						t.Fatal(err)
					}
					if state.Status == "running" {
						if !planned || active != "" || state.ID != fmt.Sprintf("group-%d", completed+1) {
							t.Fatalf("group start out of order: %s", content)
						}
						active = state.ID
					} else {
						if state.Status != "completed" || active != state.ID {
							t.Fatalf("group end out of order: %s", content)
						}
						active = ""
						completed++
					}
				case "model_request", "model_delta", "model_response", "tool_start", "tool_end", "group_report":
					var event struct {
						GroupID string `json:"group_id"`
					}
					if err := json.Unmarshal([]byte(content), &event); err != nil {
						t.Fatal(err)
					}
					if active == "" || event.GroupID != active {
						t.Fatalf("unscoped group event: %s %s", role, content)
					}
				case "assistant":
					if active != "" || completed != 3 {
						t.Fatal("merged report must be outside every group")
					}
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			if completed != 3 {
				t.Fatalf("missing group completion events: %d", completed)
			}
			p.expect(202, "POST", "/api/agent/sessions/"+sessionID+"/messages", object{"message": "继续查看缓存"}, nil)
			result = waitAgentSession(t, p, sessionID)
			if result["session"].(object)["status"] != "completed" || calls.Load() != 10 {
				t.Fatalf("followup failed: %v", result)
			}
			var jobs int
			p.db.SQL.QueryRow("SELECT count(*) FROM jobs").Scan(&jobs)
			if jobs != 2 {
				t.Fatalf("report unexpectedly started full scan: %d jobs", jobs)
			}
			p.expect(200, "DELETE", "/api/jobs/"+id, nil, nil)
			p.expect(409, "POST", "/api/agent/sessions/"+sessionID+"/messages", object{"message": "继续"}, nil)
		})
	}
}

func TestReportPlanPaginationAndRevision(t *testing.T) {
	fixture := &reportFixture{containers: fixtureContainers(53)}
	a := &Manager{records: fixture}
	containers, err := a.reportContainers(context.Background(), "record", 0)
	if err != nil || len(containers) != 53 || containers[52].ID != "container-52" {
		t.Fatalf("pagination lost containers: %d %v", len(containers), err)
	}
	fixture.revision.Store(1)
	if _, err = a.reportContainers(context.Background(), "record", 0); err == nil {
		t.Fatal("stale plan accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = a.reportContainers(ctx, "record", 1); err == nil {
		t.Fatal("cancelled plan continued")
	}
}

func TestDiskReportEmptyRecordSkipsModel(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	p.configure()
	job := p.expect(202, "POST", "/api/jobs", object{}, nil)
	id := job["id"].(string)
	waitJob(t, p.records, id)
	var calls atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer mock.Close()
	configureTestAgent(t, p, "completions", mock.URL)
	created := p.expect(202, "POST", "/api/agent/reports", object{"scope": "container", "snapshot_id": id, "revision": 0, "concurrency": 1}, nil)
	result := waitAgentSession(t, p, created["id"].(string))
	if result["session"].(object)["status"] != "completed" || calls.Load() != 0 || !strings.Contains(httpapi.JSONText(result), "没有容器") {
		t.Fatalf("empty record called model or lacked explanation: %v", result)
	}
}

func TestDiskReportRepairsInvalidGroupWithoutRepeatingExploration(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	p.configure()
	job := p.expect(202, "POST", "/api/jobs", object{}, nil)
	id := job["id"].(string)
	waitJob(t, p.records, id)
	fixture := &reportFixture{Records: p.records, containers: fixtureContainers(4)}
	p.agent.records = fixture
	var calls atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body object
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		switch calls.Add(1) {
		case 1:
			var toolCalls []agentToolCall
			for _, c := range fixture.containers {
				toolCalls = append(toolCalls, agentToolCall{ID: c.ID, Name: "get_container", Arguments: httpapi.JSONText(object{"container": c.ID})})
			}
			writeReportReply(w, "completions", "", toolCalls)
		case 2:
			writeReportReply(w, "completions", "分析说明\n```json\n"+httpapi.JSONText(fixtureReport(fixture.containers))+"\n```", nil)
		default:
			if !strings.Contains(httpapi.JSONText(body), "结果校验失败") || !strings.Contains(httpapi.JSONText(body), "共享的模型权重") {
				t.Error("repair lost feedback or existing findings")
			}
			writeReportReply(w, "completions", httpapi.JSONText(fixtureReport(fixture.containers)), nil)
		}
	}))
	defer mock.Close()
	configureTestAgent(t, p, "completions", mock.URL)
	created := p.expect(202, "POST", "/api/agent/reports", object{"scope": "container", "snapshot_id": id, "revision": 0, "concurrency": 1}, nil)
	sessionID := created["id"].(string)
	result := waitAgentSession(t, p, sessionID)
	if result["session"].(object)["status"] != "completed" || calls.Load() != 3 || fixture.reads.Load() != 8 {
		t.Fatalf("format repair failed or repeated exploration: %v", result["session"])
	}
	var groups, finals int
	p.db.SQL.QueryRow("SELECT count(*) FROM agent_messages WHERE session_id=? AND role='group_report'", sessionID).Scan(&groups)
	p.db.SQL.QueryRow("SELECT count(*) FROM agent_messages WHERE session_id=? AND role='assistant'", sessionID).Scan(&finals)
	if groups != 1 || finals != 1 {
		t.Fatalf("repair published duplicate/incomplete results: groups=%d finals=%d", groups, finals)
	}
}

func TestReportGroupValidation(t *testing.T) {
	group := fixtureContainers(1)
	valid := httpapi.JSONText(fixtureReport(group))
	if _, err := parseReportGroup(valid, group); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"# long markdown report", valid + "{}", `{"containers":[]}`,
		strings.Replace(valid, "container-00", "container-99", 1),
		strings.Replace(valid, `"category":2`, `"category":5`, 1),
		strings.Replace(valid, `"bytes":8192`, `"bytes":-1`, 1),
		strings.Replace(valid, `"kind":"模型权重"`, `"kind":"垃圾"`, 1),
		strings.Replace(valid, `"path":"/shared/models"`, `"path":"../models"`, 1),
		strings.Replace(valid, "尚未确定任务依赖，不能直接删除。", "", 1),
	} {
		if _, err := parseReportGroup(raw, group); err == nil {
			t.Errorf("invalid result accepted: %s", raw)
		}
	}
}

func TestReportKeepsDistinctCleanupFindings(t *testing.T) {
	group := fixtureContainers(1)
	result := fixtureReport(group)
	result.Containers[0].Findings = nil
	for i := 0; i < 9; i++ {
		finding := fixtureReport(group).Containers[0].Findings[0]
		finding.Path = fmt.Sprintf("/private/cache-%d", i)
		finding.Reason = strings.Repeat("保留具体处理条件。", 20)
		result.Containers[0].Findings = append(result.Containers[0].Findings, finding)
	}
	parsed, err := parseReportGroup(httpapi.JSONText(result), group)
	if err != nil || len(parsed.Containers[0].Findings) != 9 {
		t.Fatalf("useful findings rejected: %v", err)
	}
}

func TestReportExploresBeyondFormerRoundLimit(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "administrator", "A-test-password-123")
	p.configure()
	job := p.expect(202, "POST", "/api/jobs", object{}, nil)
	id := job["id"].(string)
	waitJob(t, p.records, id)
	fixture := &reportFixture{Records: p.records, containers: fixtureContainers(1)}
	p.agent.records = fixture
	var calls atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body object
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body["tools"].([]any)) == 0 {
			t.Error("tools removed before analysis finished")
		}
		n := calls.Add(1)
		if n <= 14 {
			writeReportReply(w, "completions", "继续核对。", []agentToolCall{{ID: fmt.Sprint(n), Name: "get_container", Arguments: `{"container":"container-00"}`}})
			return
		}
		writeReportReply(w, "completions", httpapi.JSONText(fixtureReport(fixture.containers)), nil)
	}))
	defer mock.Close()
	configureTestAgent(t, p, "completions", mock.URL)
	created := p.expect(202, "POST", "/api/agent/reports", object{"scope": "container", "snapshot_id": id, "revision": 0, "concurrency": 1}, nil)
	result := waitAgentSession(t, p, created["id"].(string))
	if result["session"].(object)["status"] != "completed" || calls.Load() != 15 {
		t.Fatalf("exploration ended prematurely: %v, requests=%d", result["session"], calls.Load())
	}
}

func TestReportMergeGroupsPurposesConflictsAndUnknownSizes(t *testing.T) {
	group := fixtureContainers(4)
	result := fixtureReport(group)
	result.Containers[0].Findings[0].Category = 1
	result.Containers[1].Findings[0].Category = 3
	result.Containers[2].Findings[0].Bytes = nil
	result.Containers[3].Findings[0].Bytes = nil
	other := result.Containers[0].Findings[0]
	other.Path, other.Kind, other.Category = "/private/cache|<x>", "下载与包缓存", 2
	result.Containers[0].Findings = append(result.Containers[0].Findings, other)
	third := other
	third.Path, third.Kind = "/shared/other-models", "模型权重"
	result.Containers[0].Findings = append(result.Containers[0].Findings, third)
	text := renderReportFindings(group, result.Containers)
	if strings.Count(text, "| /shared/models |") != 1 || !strings.Contains(text, "判断不一致") || !strings.Contains(text, "| 未知 |") {
		t.Fatalf("bad shared path merge: %s", text)
	}
	if strings.Count(text, "### 模型权重") != 1 || strings.Index(text, "/shared/other-models") > strings.Index(text, "| /shared/models |") {
		t.Fatalf("purpose/size ordering failed: %s", text)
	}
	if !strings.Contains(text, `/private/cache\|&lt;x&gt;`) {
		t.Fatalf("unsafe table path: %s", text)
	}
	for _, c := range group {
		if !strings.Contains(text, c.Name+"：/models") {
			t.Errorf("missing shared container %s", c.Name)
		}
	}
}

func TestReportCategoryTotals(t *testing.T) {
	bytes := func(n int64) *int64 { return &n }
	finding := func(p string, category int, size *int64) reportFinding {
		return reportFinding{Path: p, Category: category, Kind: "其他", Bytes: size}
	}
	text := renderReportFindings(fixtureContainers(2), []reportContainerResult{
		{ContainerID: "container-00", Findings: []reportFinding{
			finding("/cache", 1, bytes(1024)),
			finding("/cache/child", 1, bytes(512)),
			finding("/cache/unknown", 1, nil),
			finding("/cache-other", 1, bytes(1024)),
			finding("/unknown", 2, nil),
			finding("/unknown/known", 2, bytes(512)),
			finding("/cache/cross-category", 3, bytes(256)),
			finding("/conflict", 1, bytes(128)),
		}},
		{ContainerID: "container-01", Findings: []reportFinding{
			finding("/cache", 1, bytes(2048)),   // latest observation, counted once
			finding("/conflict", 3, bytes(128)), // moves into category 2
		}},
	})
	for i, want := range []string{
		"容量总计：3.0 KiB**（已知占用；",
		"容量总计：640.0 B**（已知占用；另有 1 项容量未知，未计入；",
		"容量总计：256.0 B**（已知占用；",
		"容量总计：0.0 B**（已知占用；",
	} {
		section := strings.Split(text, fmt.Sprintf("## %d.", i+1))[1]
		section = strings.Split(section, "\n## ")[0]
		if !strings.Contains(section, want) {
			t.Errorf("category %d missing %q: %s", i+1, want, section)
		}
	}
	root := []*mergedReportFinding{
		{reportFinding: finding("/", 1, bytes(2048))},
		{reportFinding: finding("/nested/deep/file", 1, bytes(1024))},
	}
	if total := reportCategoryTotal(root, 1); !strings.Contains(total, "2.0 KiB") {
		t.Fatalf("root descendant counted twice: %s", total)
	}
}

func TestReportRejectsInventedPathsMappingsAndSizes(t *testing.T) {
	group := fixtureContainers(1)
	a := &Manager{records: &reportFixture{containers: group}}
	valid := fixtureReport(group)
	if err := a.validateReportEvidence(context.Background(), "record", valid); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*reportFinding)
	}{
		{"other container", func(f *reportFinding) { f.Path = "/private/container-99/model" }},
		{"invented child", func(f *reportFinding) { f.Path += "/invented"; f.ContainerPath += "/invented" }},
		{"wrong mapping", func(f *reportFinding) { f.ContainerPath = "/wrong" }},
		{"invented size", func(f *reportFinding) { n := int64(9999); f.Bytes = &n }},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := fixtureReport(group)
			test.change(&result.Containers[0].Findings[0])
			if _, ok := a.validateReportEvidence(context.Background(), "record", result).(reportResultError); !ok {
				t.Fatal("invalid evidence was not returned for correction")
			}
		})
	}
}
