package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	web "project-alpha/dist"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

func TestRetryOnlyLatestFailedGroupRequest(t *testing.T) {
	for _, protocol := range []string{"completions", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			p := newTestPlatform(t)
			p.login(true, "administrator", "A-test-password-123")
			p.configure()
			job := p.expect(202, "POST", "/api/jobs", object{}, nil)
			recordID := job["id"].(string)
			waitJob(t, p.records, recordID)
			fixture := &reportFixture{Records: p.records, containers: fixtureContainers(17)}
			p.agent.records = fixture
			var mu sync.Mutex
			rounds := map[string]int{}
			failedInputs := map[string]string{}
			started, release := make(chan string, 5), make(chan struct{})
			otherRelease := make(chan struct{})
			var otherOnce sync.Once
			unblockOthers := func() { otherOnce.Do(func() { close(otherRelease) }) }
			defer unblockOthers()
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body object
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				identity := r.Header.Get("X-Opencode-Session")
				index := strings.LastIndex(identity, "-group-")
				if index < 0 {
					t.Error("retry started an unscoped agent")
					http.Error(w, "bad identity", 500)
					return
				}
				groupID := identity[index+1:]
				var number int
				fmt.Sscanf(groupID, "group-%d", &number)
				group := fixture.containers[(number-1)*4 : min(number*4, 17)]
				mu.Lock()
				rounds[groupID]++
				round := rounds[groupID]
				mu.Unlock()
				if round == 1 {
					calls := []agentToolCall{}
					for _, container := range group {
						calls = append(calls, agentToolCall{ID: container.ID, Name: "get_container", Arguments: httpapi.JSONText(object{"container": container.ID})})
					}
					calls = append(calls, agentToolCall{ID: "scan", Name: "scan_directory", Arguments: `{"path":"/shared/models","depth":2}`})
					writeReportReply(w, protocol, groupID+" 已完成来源查询", calls)
					return
				}
				if groupID == "group-2" && round == 2 {
					writeReportReply(w, protocol, "第二组继续核对目录", []agentToolCall{{ID: "directory", Name: "get_directory", Arguments: `{"path":"/shared/models","offset":0,"limit":20}`}})
					return
				}
				failedRound := 2
				if groupID == "group-2" {
					failedRound = 3
				}
				input := httpapi.JSONText(body)
				if groupID != "group-1" && round == failedRound {
					mu.Lock()
					failedInputs[groupID] = input
					mu.Unlock()
					w.Header().Set("Content-Type", "text/event-stream")
					if protocol == "completions" {
						fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"discarded-partial\"}}]}\n\n")
					} else {
						fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"discarded-partial\"}\n\n")
					}
					fmt.Fprint(w, "data: {\"error\":{\"message\":\"stream failed\"}}\n\n")
					return
				}
				if groupID != "group-1" {
					mu.Lock()
					want := failedInputs[groupID]
					mu.Unlock()
					if input != want || strings.Contains(input, "discarded-partial") {
						t.Error("retry did not resend the failed input exactly, including completed tool observations")
					}
				}
				if groupID == "group-2" && round == 4 {
					started <- groupID
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					http.Error(w, "failed again", 502)
					return
				}
				if number >= 3 && round == 3 {
					started <- groupID
					select {
					case <-otherRelease:
					case <-r.Context().Done():
						return
					}
				}
				writeReportReply(w, protocol, httpapi.JSONText(fixtureReport(group)), nil)
			}))
			defer mock.Close()
			defer unblockOthers()
			defer unblock()
			configureTestAgent(t, p, protocol, mock.URL)
			created := p.expect(202, "POST", "/api/agent/reports", object{"snapshot_id": recordID, "revision": 0, "concurrency": 2}, nil)
			id := created["id"].(string)
			if result := waitAgentSession(t, p, id); result["session"].(object)["status"] != "failed" {
				t.Fatal(result)
			}
			latestRequest := func(group string) string {
				t.Helper()
				var requestID string
				if err := p.db.SQL.QueryRow(`SELECT json_extract(content,'$.request_id') FROM agent_messages WHERE session_id=? AND role='model_request' AND json_extract(content,'$.group_id')=? ORDER BY id DESC LIMIT 1`, id, group).Scan(&requestID); err != nil {
					t.Fatal(err)
				}
				return requestID
			}
			target := object{"group_id": "group-2", "request_id": latestRequest("group-2")}
			route := "/api/agent/sessions/" + id + "/retry"
			p.expect(409, "POST", route, retryBatch(object{"group_id": "group-1", "request_id": latestRequest("group-1")}), nil)
			p.expect(409, "POST", route, retryBatch(object{"group_id": "group-2", "request_id": "stale"}), nil)
			p.expect(400, "POST", route, object{"group_id": "group-2"}, nil)
			p.expect(201, "POST", "/api/users", object{"username": "anotheradmin", "password": "Second-admin-pass-123", "role": "admin"}, nil)
			p.login(false, "anotheradmin", "Second-admin-pass-123")
			p.expect(404, "POST", route, retryBatch(target), nil)
			p.login(false, "administrator", "A-test-password-123")

			// No live goroutine or private checkpoint is needed after a restart.
			p.agent.Close()
			restarted, err := NewManager(NewStore(p.db), fixture)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			p.agent = restarted
			p.s = platform.NewServer(p.db, testModules{p.api, NewHandler(restarted.db, restarted)}, web.Assets, nil, false)
			batch := []object{target}
			for _, group := range []string{"group-3", "group-4", "group-5"} {
				batch = append(batch, object{"group_id": group, "request_id": latestRequest(group)})
			}
			// Validate the entire batch before starting even its first group.
			p.expect(409, "POST", route, retryBatch(target, object{"group_id": "group-3", "request_id": "stale"}), nil)
			p.expect(400, "POST", route, retryBatch(target, target), nil)
			p.expect(202, "POST", route, retryBatch(batch...), nil)
			receive := func() string {
				t.Helper()
				select {
				case group := <-started:
					return group
				case <-time.After(5 * time.Second):
					t.Fatal("retry slot did not start or refill")
					return ""
				}
			}
			initial := map[string]bool{receive(): true, receive(): true}
			if !initial["group-2"] || !initial["group-3"] {
				t.Fatalf("wrong parallel retries: %v", initial)
			}
			select {
			case extra := <-started:
				t.Fatalf("retry concurrency limit exceeded: %s", extra)
			case <-time.After(50 * time.Millisecond):
			}
			p.expect(409, "POST", route, retryBatch(target), nil)
			unblockOthers()
			if receive() != "group-4" || receive() != "group-5" {
				t.Fatal("queued retries did not refill slots while another agent was blocked")
			}
			unblock()
			if result := waitAgentSession(t, p, id); result["session"].(object)["status"] != "failed" {
				t.Fatal(result)
			}
			p.expect(409, "POST", route, retryBatch(target), nil)
			state, err := p.agent.savedReport(id)
			if err != nil || state.States["group-1"] != "completed" || state.States["group-2"] != "failed" || state.States["group-3"] != "completed" || state.States["group-4"] != "completed" || state.States["group-5"] != "completed" {
				t.Fatalf("one failed retry blocked other agents: %+v %v", state, err)
			}
			target["request_id"] = latestRequest("group-2")
			p.expect(202, "POST", route, retryBatch(target), nil)
			if result := waitAgentSession(t, p, id); result["session"].(object)["status"] != "completed" {
				t.Fatal(result)
			}
			mu.Lock()
			if rounds["group-1"] != 2 || rounds["group-2"] != 5 || rounds["group-3"] != 3 || rounds["group-4"] != 3 || rounds["group-5"] != 3 {
				t.Errorf("unexpected replays: %v", rounds)
			}
			mu.Unlock()
			if fixture.scans.Load() != 5 {
				t.Fatalf("completed scans were rerun: %d", fixture.scans.Load())
			}
			var text string
			if err := p.db.SQL.QueryRow("SELECT content FROM agent_messages WHERE session_id=? AND role='assistant' ORDER BY id DESC LIMIT 1", id).Scan(&text); err != nil || !strings.Contains(text, "17 个容器 · 5 组") {
				t.Fatalf("missing merged report: %s %v", text, err)
			}
			for _, container := range fixture.containers {
				if !strings.Contains(text, container.Name) {
					t.Errorf("report lost completed container %s", container.Name)
				}
			}
			p.expect(409, "POST", route, retryBatch(target), nil)
		})
	}
}

func retryBatch(targets ...object) object { return object{"requests": targets} }
