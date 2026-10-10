package containers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDRAMAndRootlessAuthorizationAreIndependent(t *testing.T) {
	s := newServiceFixture(t)
	original := s.handler.mountControl
	bySocket := map[string][]rootlessBinding{}
	s.handler.mountControl = func(ctx context.Context, c ServiceConfig, args []string) (string, error) {
		socket := c.ControlSocket
		s.bindings = bySocket[socket]
		if s.bindings == nil {
			s.bindings = []rootlessBinding{}
		}
		out, err := original(ctx, c, args)
		bySocket[socket] = s.bindings
		return out, err
	}
	configure := func(name string, c ServiceConfig) (int, string) {
		s.service.Config.Labels["project-alpha.service"] = name
		raw, _ := json.Marshal(c)
		return call(s.handler, "PUT", servicesPath+"/"+name+"/settings", string(raw), admin)
	}
	root, dram := serviceConfigDefault(rootlessService), serviceConfigDefault("dram-bw")
	if len(dram.Users) != 0 {
		t.Fatal("DRAM must not grant access by default")
	}
	root.Users = []string{"alice"}
	dram.Users = []string{"alice"}
	for name, c := range map[string]ServiceConfig{rootlessService: root, "dram-bw": dram} {
		if code, out := configure(name, c); code != 200 {
			t.Fatal(name, code, out)
		}
	}
	mounts, err := s.handler.serviceMounts()
	if err != nil || len(mounts) != 2 {
		t.Fatal(mounts, err)
	}
	s.fail = "remove"
	dram.Users = []string{}
	if code, _ := configure("dram-bw", dram); code == 200 {
		t.Fatal("failed revocation succeeded")
	}
	saved, err := s.handler.serviceConfig("dram-bw")
	if err != nil || len(saved.Users) != 1 {
		t.Fatal(saved, err)
	}
	if _, err = s.handler.db.SQL.Exec("UPDATE managed_containers SET owner='bob'"); err == nil {
		t.Fatal("owner changed while access remained")
	}
	s.fail = ""
	if code, out := configure("dram-bw", dram); code != 200 {
		t.Fatal(code, out)
	}
	mounts, err = s.handler.serviceMounts()
	if err != nil || len(mounts) != 1 || mounts[0].Service != rootlessService {
		t.Fatal("DRAM revocation affected Rootless", mounts, err)
	}
	if len(bySocket[root.ControlSocket]) != 1 || len(bySocket[dram.ControlSocket]) != 0 {
		t.Fatal(bySocket)
	}
	// Reject an overlap before an add can cover another service's socket.
	dram.Users = []string{"alice"}
	dram.SocketPath = root.SocketPath
	if code, out := configure("dram-bw", dram); code == 200 || !strings.Contains(out, "目标路径已被其他服务使用") {
		t.Fatal(code, out)
	}
	mounts, err = s.handler.serviceMounts()
	if err != nil || len(mounts) != 1 || mounts[0].Service != rootlessService {
		t.Fatal(mounts, err)
	}
}

func TestDRAMFailedAttachmentKeepsOwnershipProtection(t *testing.T) {
	s := newServiceFixture(t)
	s.service.Config.Labels["project-alpha.service"] = "dram-bw"
	c := serviceConfigDefault("dram-bw")
	c.Users = []string{"alice"}
	s.handler.mountControl = func(_ context.Context, _ ServiceConfig, args []string) (string, error) {
		if args[0] == "add" {
			return "", errors.New("mount failed")
		}
		return "ready", nil
	}
	raw, _ := json.Marshal(c)
	if code, _ := call(s.handler, "PUT", servicesPath+"/dram-bw/settings", string(raw), admin); code == 200 {
		t.Fatal("failed mount succeeded")
	}
	mounts, err := s.handler.serviceMounts()
	if err != nil || len(mounts) != 1 || mounts[0].Service != "dram-bw" || mounts[0].Error == "" {
		t.Fatal(mounts, err)
	}
	if err = s.handler.checkServiceMounts(s.docker.c.ID); err == nil {
		t.Fatal("failed mount lost protection")
	}
}

func TestDRAMSamplingRepairDoesNotNeedCrashedManager(t *testing.T) {
	s := newServiceFixture(t)
	s.service.Config.Labels["project-alpha.service"] = "dram-bw"
	c := serviceConfigDefault("dram-bw")
	c.Users = []string{"alice"}
	raw, _ := json.Marshal(c)
	if code, out := call(s.handler, "PUT", servicesPath+"/dram-bw/settings", string(raw), admin); code != 200 {
		t.Fatal(code, out)
	}
	s.service.State.Restarting = true
	s.service.State.Status = "restarting"
	original := s.handler.mountControl
	s.handler.mountControl = func(ctx context.Context, c ServiceConfig, args []string) (string, error) {
		if s.service.State.Restarting {
			return "", errors.New("manager unavailable while collector crashes")
		}
		return original(ctx, c, args)
	}
	c.DRAM.Backend = "mock"
	raw, _ = json.Marshal(c)
	if code, out := call(s.handler, "PUT", servicesPath+"/dram-bw/settings", string(raw), admin); code != 200 {
		t.Fatal("sampling repair blocked by prior mounts", code, out)
	}
	mounts, err := s.handler.serviceMounts()
	if err != nil || len(mounts) != 1 || mounts[0].Error != "" {
		t.Fatal(mounts, err)
	}
}
