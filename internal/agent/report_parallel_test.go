package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"project-alpha/internal/httpapi"
)

// Hold publication briefly so overlapping scan attempts are observable.
type parallelReportFixture struct {
	*reportFixture
	exploring atomic.Int32
	overlap   atomic.Bool
}

func (f *parallelReportFixture) Explore(actor, id, path string, revision int64, depth int) (object, error) {
	if f.exploring.Add(1) != 1 {
		f.overlap.Store(true)
	}
	job, err := f.reportFixture.Explore(actor, id, path, revision, depth)
	if err != nil {
		f.exploring.Add(-1)
	}
	return job, err
}
func (f *parallelReportFixture) Wait(ctx context.Context, id string, check func() error) error {
	defer f.exploring.Add(-1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(20 * time.Millisecond):
	}
	return f.reportFixture.Wait(ctx, id, check)
}

func TestParallelReportRefillsSlotsWithFreshAgents(t *testing.T) {
	for _, outcome := range []string{"completed", "failed", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			p := newTestPlatform(t)
			p.Login(true, "administrator", "A-test-password-123")
			p.configure()
			job := p.Expect(202, "POST", "/api/jobs", object{}, nil)
			recordID := job["id"].(string)
			waitJob(t, p.records, recordID)
			fixture := &parallelReportFixture{reportFixture: &reportFixture{Records: p.records, containers: fixtureContainers(13)}}
			p.agent.records = fixture
			started := make(chan int, 8)
			var gates [4]chan struct{}
			var releases [4]sync.Once
			for i := range gates {
				gates[i] = make(chan struct{})
			}
			release := func(i int) { releases[i].Do(func() { close(gates[i]) }) }
			var mu sync.Mutex
			rounds := map[int]int{}
			identities := map[int]string{}
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				identity := r.Header.Get("x-opencode-session")
				at := strings.LastIndex(identity, "-group-")
				if at < 0 {
					t.Error("missing independent group identity")
					http.Error(w, "bad identity", 500)
					return
				}
				number, err := strconv.Atoi(identity[at+7:])
				if err != nil || number < 1 || number > 4 {
					t.Error("bad group identity")
					http.Error(w, "bad group", 500)
					return
				}
				index := number - 1
				var body object
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				round := rounds[index]
				rounds[index]++
				identities[index] = identity
				mu.Unlock()
				group := fixture.containers[index*4 : min(index*4+4, 13)]
				encoded := httpapi.JSONText(body)
				if round == 0 {
					history, ok := body["messages"].([]any)
					if !ok || len(history) != 2 {
						t.Errorf("agent %d inherited previous conversation: %s", number, encoded)
					}
					for _, container := range group {
						if !strings.Contains(encoded, container.ID) {
							t.Errorf("agent %d missing assigned container", number)
						}
					}
					for other := 0; other < 4; other++ {
						if other != index && strings.Contains(encoded, fmt.Sprintf("agent-%d-private-note", other+1)) {
							t.Error("another agent's conversation was reused")
						}
					}
					started <- index
					select {
					case <-r.Context().Done():
						return
					case <-gates[index]:
					}
					if outcome == "failed" && index == 1 {
						http.Error(w, "group two failed", http.StatusBadGateway)
						return
					}
					calls := []agentToolCall{}
					for _, container := range group {
						calls = append(calls, agentToolCall{ID: container.ID, Name: "get_container", Arguments: httpapi.JSONText(object{"container": container.ID})})
					}
					calls = append(calls, agentToolCall{ID: "shared-call-id", Name: "scan_directory", Arguments: `{"path":"/shared/models","depth":2}`})
					writeReportReply(w, "completions", fmt.Sprintf("agent-%d-private-note", number), calls)
				} else {
					if round != 1 {
						t.Errorf("unexpected retry for agent %d: %d", number, round)
					}
					if !strings.Contains(encoded, fmt.Sprintf("agent-%d-private-note", number)) {
						t.Errorf("agent %d lost its own context", number)
					}
					writeReportReply(w, "completions", httpapi.JSONText(fixtureReport(group)), nil)
				}
			}))
			defer mock.Close()
			defer func() {
				for i := range gates {
					release(i)
				}
			}()
			configureTestAgent(t, p, "completions", mock.URL)
			created := p.Expect(202, "POST", "/api/agent/reports", object{"scope": "container", "snapshot_id": recordID, "revision": 0, "concurrency": 2}, nil)
			sessionID := created["id"].(string)
			receive := func() int {
				t.Helper()
				select {
				case index := <-started:
					return index
				case <-time.After(5 * time.Second):
					t.Fatal("next agent did not fill a free slot")
					return -1
				}
			}
			initial := map[int]bool{receive(): true, receive(): true}
			if !initial[0] || !initial[1] {
				t.Fatalf("incorrect initial agents: %v", initial)
			}
			select {
			case extra := <-started:
				t.Fatalf("limit exceeded before a slot was freed: %d", extra)
			case <-time.After(50 * time.Millisecond):
			}
			if outcome == "cancelled" {
				p.Expect(200, "POST", "/api/agent/sessions/"+sessionID+"/cancel", object{}, nil)
			} else {
				// Agent 1 remains blocked while 2 completes (or fails), then 3 fills its
				// slot, then 4 fills 3's slot. This rules out batch/barrier scheduling.
				release(1)
				if index := receive(); index != 2 {
					t.Fatalf("expected a fresh agent 3, got %d", index+1)
				}
				release(2)
				if index := receive(); index != 3 {
					t.Fatalf("expected a fresh agent 4, got %d", index+1)
				}
				release(3)
				release(0)
			}
			result := waitAgentSession(t, p, sessionID)
			if result["session"].(object)["status"] != outcome {
				t.Fatalf("wrong terminal state: %v", result)
			}
			mu.Lock()
			count := len(rounds)
			mu.Unlock()
			want := 4
			if outcome == "cancelled" {
				want = 2
			}
			if count != want {
				t.Fatalf("started %d agents, want %d", count, want)
			}
			if fixture.overlap.Load() {
				t.Fatal("parallel agents published concurrent record scans")
			}
			if outcome == "completed" && fixture.scans.Load() != 4 {
				t.Fatalf("scans lost or rejected: %d", fixture.scans.Load())
			}
			rows, err := p.db.SQL.Query("SELECT role,content FROM agent_messages WHERE session_id=? ORDER BY id", sessionID)
			if err != nil {
				t.Fatal(err)
			}
			active := map[string]bool{}
			seen := map[string]bool{}
			for rows.Next() {
				var role, content string
				if err := rows.Scan(&role, &content); err != nil {
					t.Fatal(err)
				}
				var event struct {
					ID, Status string
					GroupID    string `json:"group_id"`
				}
				if role == "group_state" {
					if err := json.Unmarshal([]byte(content), &event); err != nil {
						t.Fatal(err)
					}
					if event.Status == "running" {
						if seen[event.ID] {
							t.Fatal("agent identity reused")
						}
						seen[event.ID] = true
						active[event.ID] = true
						if len(active) > 2 {
							t.Fatal("running agent limit exceeded")
						}
					} else {
						delete(active, event.ID)
					}
				}
				if role == "model_request" || role == "model_delta" || role == "model_response" || role == "tool_start" || role == "tool_end" || role == "group_report" {
					if err := json.Unmarshal([]byte(content), &event); err != nil {
						t.Fatal(err)
					}
					if !active[event.GroupID] {
						t.Fatalf("event lost its active agent: %s %s", role, content)
					}
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			if len(active) != 0 {
				t.Fatalf("agents still running after session ended: %v", active)
			}
		})
	}
}
