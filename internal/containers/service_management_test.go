package containers

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"project-alpha/internal/platform"
)

type serviceFixture struct {
	handler   *Handler
	docker    *fakeDocker
	service   inspection
	bindings  []rootlessBinding
	calls     []string
	fail      string
	duplicate bool
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	h, f, cfg := fixture(t)
	adopt(t, h)
	if _, err := h.db.SQL.Exec("UPDATE managed_containers SET owner='alice'"); err != nil {
		t.Fatal(err)
	}
	s := &serviceFixture{handler: h, docker: f, bindings: []rootlessBinding{}}
	s.service.ID = strings.Repeat("b", 64)
	s.service.Config.Labels = map[string]string{"project-alpha.service": rootlessService, "project-alpha.dram-settings": "1"}
	s.service.Config.Entrypoint = []string{"/usr/local/bin/dram-bwd", "--settings", "/run/dram-bw/service-settings"}
	s.service.State.Running = true
	s.service.State.Status = "running"
	h.run = func(ctx context.Context, endpoint string, args []string, input string) (string, error) {
		if args[0] == "container" && args[1] == "ls" {
			if slices.Contains(args, "--no-trunc") {
				if s.duplicate {
					return s.service.ID + "\n" + strings.Repeat("c", 64), nil
				}
				return s.service.ID, nil
			}
			return fmt.Sprintf(`{"name":"rootless-docker","state":%q}`, s.service.State.Status), nil
		}
		if args[0] == "container" && args[1] == "inspect" && args[2] == s.service.ID {
			raw, _ := json.Marshal([]inspection{s.service})
			return string(raw), nil
		}
		return f.run(ctx, endpoint, args, input)
	}
	h.serviceRun = func(ctx context.Context, endpoint string, args []string, input string) (string, error) {
		if endpoint != cfg.Endpoint {
			t.Fatal("wrong endpoint", endpoint)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < time.Minute {
			t.Fatal("service shutdown deadline too short")
		}
		s.calls = append(s.calls, "docker "+strings.Join(args, " "))
		if s.fail == args[0] {
			return "", errors.New("injected " + args[0])
		}
		if args[0] == "stop" {
			s.service.State.Running = false
			s.service.State.Restarting = false
			s.service.State.Status = "exited"
		}
		if args[0] == "start" || args[0] == "restart" {
			s.service.State.Running = true
			s.service.State.Restarting = false
			s.service.State.Status = "running"
		}
		return "", nil
	}
	h.rootlessControl = func(ctx context.Context, socket string, args []string) (string, error) {
		s.calls = append(s.calls, "rootless "+strings.Join(args, " "))
		if s.fail == args[0] {
			return "", errors.New("injected " + args[0])
		}
		switch args[0] {
		case "bindings":
			raw, _ := json.Marshal(s.bindings)
			return string(raw), nil
		case "status":
			return "ready", nil
		case "add":
			// Check the exact CLI wire shape; IDs are positional, never container names.
			if len(args) != 6 || args[1] != "--host" || args[3] != f.c.ID || args[4] != "--socket-path" {
				t.Fatal(args)
			}
			m := rootlessBinding{Host: args[2], Container: args[3], SocketPath: args[5], State: "mounted"}
			if !slices.ContainsFunc(s.bindings, func(b rootlessBinding) bool { return b == m }) {
				s.bindings = append(s.bindings, m)
			}
			return "", nil
		case "remove":
			s.bindings = slices.DeleteFunc(s.bindings, func(b rootlessBinding) bool {
				return b.Host == args[2] && b.Container == args[3] && b.SocketPath == args[5]
			})
			return "", nil
		}
		return "", fmt.Errorf("unexpected rootless args %v", args)
	}
	return s
}
func (s *serviceFixture) configure(t *testing.T, users ...string) (int, string) {
	t.Helper()
	c := serviceConfigDefault(rootlessService)
	c.Users = append([]string{}, users...)
	raw, _ := json.Marshal(c)
	return call(s.handler, "PUT", servicesPath+"/rootless-docker/settings", string(raw), admin)
}
func TestServiceLifecycleRestoresSelectedUsers(t *testing.T) {
	s := newServiceFixture(t)
	if code, out := s.configure(t, "alice"); code != 200 {
		t.Fatal(code, out)
	}
	if len(s.bindings) != 1 {
		t.Fatal(s.bindings)
	}
	for _, action := range []string{"restart", "stop", "start", "apply"} {
		s.calls = nil
		if code, out := call(s.handler, "POST", servicesPath+"/rootless-docker/"+action, "{}", admin); code != 200 {
			t.Fatal(action, code, out)
		}
		mounts, err := s.handler.serviceMounts()
		if err != nil {
			t.Fatal(err)
		}
		if action == "stop" {
			if len(s.bindings) != 0 || len(mounts) != 0 {
				t.Fatal("stop did not revoke mounts")
			}
		} else if len(s.bindings) != 1 || len(mounts) != 1 {
			t.Fatal("did not restore mount", action, s.bindings, mounts)
		}
		if action == "restart" || action == "stop" {
			revoke := slices.IndexFunc(s.calls, func(v string) bool { return strings.HasPrefix(v, "rootless remove ") })
			shutdown := slices.IndexFunc(s.calls, func(v string) bool { return strings.HasPrefix(v, "docker "+action+" --time 120 ") })
			if revoke < 0 || shutdown <= revoke {
				t.Fatal("must revoke before shutdown with full grace period", s.calls)
			}
		}
	}
	if code, out := s.configure(t); code != 200 {
		t.Fatal(code, out)
	}
	if len(s.bindings) != 0 {
		t.Fatal("deselection retained access")
	}
	if _, err := s.handler.db.SQL.Exec("UPDATE managed_containers SET owner='bob'"); err != nil {
		t.Fatal(err)
	}
}
func TestServiceRevocationFailurePreservesJournalAndOwnership(t *testing.T) {
	s := newServiceFixture(t)
	if code, out := s.configure(t, "alice"); code != 200 {
		t.Fatal(code, out)
	}
	s.fail = "remove"
	if code, _ := s.configure(t); code == 200 {
		t.Fatal("failed removal succeeded")
	}
	cfg, err := s.handler.serviceConfig(rootlessService)
	if err != nil || !slices.Contains(cfg.Users, "alice") {
		t.Fatal("lost prior desired config", cfg, err)
	}
	mounts, err := s.handler.serviceMounts()
	if err != nil || len(mounts) != 1 || mounts[0].Error == "" {
		t.Fatal(mounts, err)
	}
	if _, err = s.handler.db.SQL.Exec("UPDATE managed_containers SET owner='bob'"); err == nil {
		t.Fatal("mounted container reassigned")
	}
	s.calls = nil
	if code, _ := call(s.handler, "POST", servicesPath+"/rootless-docker/stop", "{}", admin); code == 200 {
		t.Fatal("stopped despite failed revoke")
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "docker stop") {
			t.Fatal(c)
		}
	}
	s.fail = ""
	if code, out := s.configure(t); code != 200 {
		t.Fatal(code, out)
	}
	if _, err = s.handler.db.SQL.Exec("UPDATE managed_containers SET owner='bob'"); err != nil {
		t.Fatal(err)
	}
}
func TestServiceAttachFailurePersistsIntentAndRetriesAfterRestart(t *testing.T) {
	s := newServiceFixture(t)
	s.fail = "add"
	if code, _ := s.configure(t, "alice"); code == 200 {
		t.Fatal("attach failure ignored")
	}
	mounts, err := s.handler.serviceMounts()
	if err != nil || len(mounts) != 1 || mounts[0].Error == "" {
		t.Fatal(mounts, err)
	}
	s.fail = ""
	// Recreate the handler as a worker restart would; recover from database intent.
	next := NewHandler(s.handler.db)
	next.run = s.handler.run
	next.rootlessControl = s.handler.rootlessControl
	if err = next.ReconcileServices(context.Background()); err != nil {
		t.Fatal(err)
	}
	mounts, err = next.serviceMounts()
	if err != nil || len(mounts) != 1 || mounts[0].Error != "" || len(s.bindings) != 1 {
		t.Fatal(mounts, s.bindings, err)
	}
	if err = next.ReconcileServices(context.Background()); err != nil || len(s.bindings) != 1 {
		t.Fatal("non-idempotent recovery", err)
	}
}
func TestServiceBoundaryAndImmutableIdentity(t *testing.T) {
	s := newServiceFixture(t)
	for _, method := range []string{"GET", "POST", "PUT"} {
		if code, _ := call(s.handler, method, servicesPath, "{}", platform.User{Role: "viewer"}); code == 200 {
			t.Fatal("viewer permitted")
		}
	}
	s.duplicate = true
	if code, _ := call(s.handler, "POST", servicesPath+"/rootless-docker/restart", "{}", admin); code == 200 {
		t.Fatal("ambiguous service controlled")
	}
	if len(s.calls) != 0 {
		t.Fatal(s.calls)
	}
	s.duplicate = false
	s.service.Config.Labels["project-alpha.service"] = "other"
	if code, _ := call(s.handler, "POST", servicesPath+"/rootless-docker/start", "{}", admin); code == 200 {
		t.Fatal("label mismatch ignored")
	}
	if len(s.calls) != 0 {
		t.Fatal(s.calls)
	}
	c := serviceConfigDefault("tetragon")
	c.Users = []string{"alice"}
	raw, _ := json.Marshal(c)
	if code, _ := call(s.handler, "PUT", servicesPath+"/tetragon/settings", string(raw), admin); code == 200 {
		t.Fatal("exposed monitoring socket")
	}
}
func TestServiceObservationNeverTrustsOfflineRecords(t *testing.T) {
	s := newServiceFixture(t)
	if code, out := s.configure(t, "alice"); code != 200 {
		t.Fatal(code, out)
	}
	s.service.State.Running = false
	s.service.State.Status = "exited"
	code, out := call(s.handler, "GET", servicesPath, "", admin)
	if code != 200 || !strings.Contains(out, `"state":"unknown"`) || strings.Contains(out, `"state":"mounted"`) {
		t.Fatal(code, out)
	}
	s.service.State.Running = true
	s.service.State.Status = "running"
	s.fail = "bindings"
	code, out = call(s.handler, "GET", servicesPath, "", admin)
	if code != 200 || !strings.Contains(out, "injected bindings") || strings.Contains(out, `"state":"mounted"`) {
		t.Fatal(code, out)
	}
}
func TestMountedContainerCannotBeDeletedBeforeRevocation(t *testing.T) {
	s := newServiceFixture(t)
	if code, out := s.configure(t, "alice"); code != 200 {
		t.Fatal(code, out)
	}
	s.docker.c.State.Running = false
	s.docker.c.State.Status = "exited"
	s.docker.calls = nil
	if code, _ := call(s.handler, "POST", "/api/containers/"+s.docker.c.ID+"/delete", `{"confirm":"alice"}`, admin); code == 200 {
		t.Fatal("deleted mounted container")
	}
	for _, args := range s.docker.calls {
		if args[0] == "rm" {
			t.Fatal("deleted before checking journal")
		}
	}
}

func TestMonitoringServicesHaveIndependentSettingsAndControls(t *testing.T) {
	s := newServiceFixture(t)
	for _, name := range []string{"tetragon", "dram-bw"} {
		s.service.Config.Labels["project-alpha.service"] = name
		c := serviceConfigDefault(name)
		c.StopTimeout = 35
		c.RestartPolicy = "on-failure"
		raw, _ := json.Marshal(c)
		if code, out := call(s.handler, "PUT", servicesPath+"/"+name+"/settings", string(raw), admin); code != 200 {
			t.Fatal(code, out)
		}
		for _, action := range []string{"stop", "start", "restart"} {
			if code, out := call(s.handler, "POST", servicesPath+"/"+name+"/"+action, "{}", admin); code != 200 {
				t.Fatal(code, out)
			}
		}
		saved, err := s.handler.serviceConfig(name)
		if err != nil || saved.StopTimeout != 35 || saved.RestartPolicy != "on-failure" {
			t.Fatal(saved, err)
		}
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "rootless ") {
			t.Fatal("monitoring service touched rootless mounts", c)
		}
	}
	rootless, err := s.handler.serviceConfig(rootlessService)
	if err != nil || rootless.StopTimeout != 120 {
		t.Fatal("settings leaked across services", rootless, err)
	}
}
func TestServiceDestinationChangeRevokesOldMountAndStopFailureIsRecoverable(t *testing.T) {
	s := newServiceFixture(t)
	if code, out := s.configure(t, "alice"); code != 200 {
		t.Fatal(code, out)
	}
	cfg := serviceConfigDefault(rootlessService)
	cfg.Users = []string{"alice"}
	cfg.SocketPath = "/run/custom.sock"
	raw, _ := json.Marshal(cfg)
	if code, out := call(s.handler, "PUT", servicesPath+"/rootless-docker/settings", string(raw), admin); code != 200 {
		t.Fatal(code, out)
	}
	if len(s.bindings) != 1 || s.bindings[0].SocketPath != "/run/custom.sock" {
		t.Fatal("old socket retained", s.bindings)
	}
	s.fail = "restart"
	if code, _ := call(s.handler, "POST", servicesPath+"/rootless-docker/restart", "{}", admin); code == 200 {
		t.Fatal("failed restart succeeded")
	}
	if len(s.bindings) != 0 {
		t.Fatal("restarted before revocation")
	}
	saved, err := s.handler.serviceConfig(rootlessService)
	if err != nil || len(saved.Users) != 1 {
		t.Fatal("restart failure lost selection", saved, err)
	}
	s.fail = ""
	if code, out := call(s.handler, "POST", servicesPath+"/rootless-docker/apply", "{}", admin); code != 200 {
		t.Fatal(code, out)
	}
	if len(s.bindings) != 1 {
		t.Fatal("retry failed", s.bindings)
	}
}

func TestInvalidBindingResponseCannotClearJournal(t *testing.T) {
	s := newServiceFixture(t)
	if code, out := s.configure(t, "alice"); code != 200 {
		t.Fatal(code, out)
	}
	s.handler.rootlessControl = func(context.Context, string, []string) (string, error) { return `[{}]`, nil }
	if code, _ := s.configure(t); code == 200 {
		t.Fatal("malformed observations accepted")
	}
	mounts, err := s.handler.serviceMounts()
	if err != nil || len(mounts) != 1 {
		t.Fatal("journal cleared", mounts, err)
	}
}

func TestDRAMSettingsReachDaemonAndStoppedServiceStaysStopped(t *testing.T) {
	s := newServiceFixture(t)
	s.service.Config.Labels["project-alpha.service"] = "dram-bw"
	s.service.State.Running = false
	s.service.State.Status = "exited"
	original := s.handler.serviceRun
	var payload string
	s.handler.serviceRun = func(ctx context.Context, endpoint string, args []string, input string) (string, error) {
		if args[0] == "cp" {
			if !slices.Equal(args, []string{"cp", "-", s.service.ID + ":/run/dram-bw/"}) {
				t.Fatal(args)
			}
			reader := tar.NewReader(strings.NewReader(input))
			header, err := reader.Next()
			if err != nil || header.Name != "service-settings" || header.Mode != 0644 {
				t.Fatal(header, err)
			}
			raw, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			payload = string(raw)
		}
		return original(ctx, endpoint, args, input)
	}
	c := serviceConfigDefault("dram-bw")
	c.DRAM = &DRAMConfig{Backend: "mock", IntervalUS: 250000, PeakGBPS: 42.5}
	raw, _ := json.Marshal(c)
	if code, out := call(s.handler, "PUT", servicesPath+"/dram-bw/settings", string(raw), admin); code != 200 {
		t.Fatal(code, out)
	}
	if payload != "mock 250000 42.5\n" || s.service.State.Running {
		t.Fatal(payload, s.calls)
	}
	for _, action := range []string{"start", "restart"} {
		payload = ""
		if code, out := call(s.handler, "POST", servicesPath+"/dram-bw/"+action, "{}", admin); code != 200 {
			t.Fatal(code, out)
		}
		if payload != "mock 250000 42.5\n" || !s.service.State.Running {
			t.Fatal(payload, s.calls)
		}
	}
	s.fail = "cp"
	if code, _ := call(s.handler, "PUT", servicesPath+"/dram-bw/settings", string(raw), admin); code == 200 {
		t.Fatal("copy failure hidden")
	}
	s.fail = ""
	delete(s.service.Config.Labels, "project-alpha.dram-settings")
	s.calls = nil
	if code, _ := call(s.handler, "PUT", servicesPath+"/dram-bw/settings", string(raw), admin); code == 200 {
		t.Fatal("old image accepted")
	}
	for _, call := range s.calls {
		if strings.HasPrefix(call, "docker cp") || strings.HasPrefix(call, "docker restart") {
			t.Fatal(call)
		}
	}
}

func TestDRAMRejectsInvalidParameters(t *testing.T) {
	for _, d := range []*DRAMConfig{nil, {Backend: "other", IntervalUS: 100000}, {Backend: "auto", IntervalUS: 99}, {Backend: "mock", IntervalUS: 10000001}, {Backend: "mock", IntervalUS: 100, PeakGBPS: -1}, {Backend: "mock", IntervalUS: 100, PeakGBPS: 1000001}} {
		c := serviceConfigDefault("dram-bw")
		c.DRAM = d
		if c.validate("dram-bw") == nil {
			t.Fatal(d)
		}
	}
}

func TestContainerCredentialsSurviveServiceMountFailure(t *testing.T) {
	for _, action := range []string{"create", "initialize"} {
		t.Run(action, func(t *testing.T) {
			s := newServiceFixture(t)
			c := serviceConfigDefault(rootlessService)
			c.Users = []string{"alice"}
			if err := s.handler.saveServiceConfig(rootlessService, c, "admin"); err != nil {
				t.Fatal(err)
			}
			path, body, wantStatus := "/api/containers/"+s.docker.c.ID+"/initialize", "{}", 200
			query := "UPDATE managed_containers SET initialized=0,gate='/run/test-ready'"
			if action == "create" {
				query = "DELETE FROM managed_containers"
				s.docker.absent = true
				path, body, wantStatus = "/api/containers", `{"name":"training","owner":"alice"}`, 201
			}
			if _, err := s.handler.db.SQL.Exec(query); err != nil {
				t.Fatal(err)
			}
			s.fail = "add"
			code, out := call(s.handler, "POST", path, body, admin)
			if code != wantStatus {
				t.Fatalf("successful %s returned status %d", action, code)
			}
			var result struct{ Password, Warning string }
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatal(err)
			}
			if result.Password == "" || !slices.Contains(s.docker.inputs, "root:"+result.Password+"\n") {
				t.Fatal("response lost the password installed in the container")
			}
			if !strings.Contains(result.Warning, "injected add") {
				t.Fatal("mount failure was not reported separately")
			}
			records, err := s.handler.records()
			if err != nil || len(records) != 1 || !records[0].Initialized {
				t.Fatal("initialization did not commit", err)
			}
			mounts, err := s.handler.serviceMounts()
			if err != nil || len(mounts) != 1 || mounts[0].Error == "" {
				t.Fatal("failed attachment was not retained for retry", err)
			}
			s.fail = ""
			if code, out := call(s.handler, "POST", servicesPath+"/rootless-docker/apply", "{}", admin); code != 200 {
				t.Fatal(code, out)
			}
			if len(s.bindings) != 1 {
				t.Fatal("attachment retry failed")
			}
		})
	}
}

func TestContainerMountSynchronizationIsScoped(t *testing.T) {
	s := newServiceFixture(t)
	c := serviceConfigDefault(rootlessService)
	c.Users = []string{"alice", "bob"}
	if err := s.handler.saveServiceConfig(rootlessService, c, "admin"); err != nil {
		t.Fatal(err)
	}
	// An unrelated stale container must not be inspected or attached on Alice's start.
	if _, err := s.handler.db.SQL.Exec(`INSERT INTO managed_containers
 SELECT ?,endpoint,daemon,'missing','bob',spec,'stale',origin,gate,initialized,created_at FROM managed_containers`, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if code, out := call(s.handler, "POST", "/api/containers/"+s.docker.c.ID+"/start", "{}", admin); code != 200 || strings.Contains(out, "warning") {
		t.Fatal(code, out)
	}
	mounts, err := s.handler.serviceMounts()
	if err != nil || len(mounts) != 1 || mounts[0].ContainerID != s.docker.c.ID {
		t.Fatal("synchronized an unrelated container", mounts, err)
	}
	s.calls = nil
	s.fail = "add"
	if code, out := call(s.handler, "POST", "/api/containers/"+s.docker.c.ID+"/stop", "{}", admin); code != 200 || strings.Contains(out, "warning") {
		t.Fatal(code, out)
	}
	cfg, err := s.handler.config()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(cfg)
	if code, out := call(s.handler, "PUT", "/api/containers/settings", string(raw), admin); code != 200 {
		t.Fatal(code, out)
	}
	if len(s.calls) != 0 {
		t.Fatal("unrelated operations invoked Rootless", s.calls)
	}
}

func TestDRAMCrashLoopRecovery(t *testing.T) {
	for _, action := range []string{"settings", "stop", "restart"} {
		t.Run(action, func(t *testing.T) {
			s := newServiceFixture(t)
			s.service.Config.Labels["project-alpha.service"] = "dram-bw"
			s.service.State.Restarting = true
			s.service.State.Status = "restarting"
			c := serviceConfigDefault("dram-bw")
			c.DRAM.Backend = "mock"
			raw, _ := json.Marshal(c)
			method, body := "POST", "{}"
			if action == "settings" {
				method, body = "PUT", string(raw)
			}
			if code, out := call(s.handler, method, servicesPath+"/dram-bw/"+action, body, admin); code != 200 {
				t.Fatal(code, out)
			}
			stop := slices.Index(s.calls, "docker stop --time 10 "+s.service.ID)
			copy := slices.Index(s.calls, "docker cp - "+s.service.ID+":/run/dram-bw/")
			restart := slices.Index(s.calls, "docker restart --time 10 "+s.service.ID)
			if action == "stop" {
				if stop != 0 || len(s.calls) != 1 || s.service.State.Running {
					t.Fatal("stop did not end the restart loop", s.calls)
				}
			} else if stop < 0 || copy <= stop || restart <= copy || !s.service.State.Running || s.service.State.Restarting {
				t.Fatal("configuration was not installed between stop and restart", s.calls)
			}
		})
	}
}

func TestDRAMCrashLoopRecoveryFailure(t *testing.T) {
	for _, failure := range []string{"stop", "cp", "restart", "exits-again"} {
		t.Run(failure, func(t *testing.T) {
			s := newServiceFixture(t)
			s.service.Config.Labels["project-alpha.service"] = "dram-bw"
			s.service.State.Restarting = true
			s.service.State.Status = "restarting"
			s.fail = failure
			original := s.handler.serviceRun
			s.handler.serviceRun = func(ctx context.Context, endpoint string, args []string, input string) (string, error) {
				out, err := original(ctx, endpoint, args, input)
				if failure == "exits-again" && args[0] == "restart" {
					s.service.State.Restarting = true
					s.service.State.Status = "restarting"
				}
				return out, err
			}
			c := serviceConfigDefault("dram-bw")
			c.DRAM.Backend = "mock"
			raw, _ := json.Marshal(c)
			if code, out := call(s.handler, "PUT", servicesPath+"/dram-bw/settings", string(raw), admin); code == 200 || !strings.Contains(out, "配置已保存") {
				t.Fatal(code, out)
			}
			if failure == "stop" && len(s.calls) != 1 {
				t.Fatal("continued after failed stop", s.calls)
			}
			if failure == "cp" && s.service.State.Running {
				t.Fatal("restarted despite copy failure")
			}
			s.fail = ""
			s.handler.serviceRun = original
			if code, out := call(s.handler, "POST", servicesPath+"/dram-bw/start", "{}", admin); code != 200 {
				t.Fatal("saved settings could not be retried", code, out)
			}
		})
	}
}
