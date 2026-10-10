package containers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

func TestMemberPasswordResetOwnershipRetryAndSecrets(t *testing.T) {
	h, f, _ := fixture(t)
	adopt(t, h)
	if _, err := h.db.SQL.Exec("UPDATE managed_containers SET owner='bob'"); err != nil {
		t.Fatal(err)
	}
	path := "/api/containers/members/" + strings.Repeat("b", 32) + "/password"
	password := "New-password-123"
	request := func(username string, user platform.User) (int, string) {
		raw, _ := json.Marshal(map[string]string{"username": username, "password": password})
		return call(h, "POST", path, string(raw), user)
	}
	f.calls, f.inputs = nil, nil
	if status, _ := request("bob", platform.User{Role: "viewer"}); status == 200 {
		t.Fatal("viewer reset allowed")
	}
	if status, body := request("alice", admin); status != 200 || !strings.Contains(body, `"updated":0`) {
		t.Fatalf("other owner %d %s", status, body)
	}
	if len(f.calls) != 0 {
		t.Fatal("touched another member's container")
	}
	f.c.State.Running = false
	if status, body := request("bob", admin); status != 200 || !strings.Contains(body, `"ok":false`) || !strings.Contains(body, "已停止") {
		t.Fatalf("stopped %d %s", status, body)
	}
	for _, args := range f.calls {
		if args[0] == "exec" || args[0] == "start" {
			t.Fatal("mutated stopped container")
		}
	}
	f.c.State.Running = true
	h.run = func(ctx context.Context, endpoint string, args []string, input string) (string, error) {
		if args[0] == "exec" {
			return "", fmt.Errorf("secret diagnostic: %s", input)
		}
		return f.run(ctx, endpoint, args, input)
	}
	if status, body := request("bob", admin); status != 200 || strings.Contains(body, password) || !strings.Contains(body, `"ok":false`) {
		t.Fatalf("failure %d %s", status, body)
	}
	h.run = f.run
	f.calls, f.inputs = nil, nil
	for range 2 {
		if status, body := request("bob", admin); status != 200 || !strings.Contains(body, `"ok":true`) || !strings.Contains(body, `"updated":1`) || strings.Contains(body, password) {
			t.Fatalf("retry %d %s", status, body)
		}
	}
	writes := 0
	for i, args := range f.calls {
		if strings.Contains(strings.Join(args, " "), password) {
			t.Fatal("password in argv")
		}
		if args[0] == "exec" {
			if !slices.Equal(args, []string{"exec", "-i", "--user", "0", f.c.ID, "chpasswd"}) || f.inputs[i] != "root:"+password+"\n" {
				t.Fatalf("unsafe write %v", args)
			}
			writes++
		}
	}
	if writes != 2 {
		t.Fatalf("writes %d", writes)
	}
	var audit string
	if err := h.db.SQL.QueryRow("SELECT detail FROM audit WHERE action='container.password.reset'").Scan(&audit); err != nil || audit != f.c.ID {
		t.Fatalf("audit %q %v", audit, err)
	}
	// Configuration drift must still refuse a password write.
	f.c.Config.Image = "changed:image"
	f.calls = nil
	if _, body := request("bob", admin); !strings.Contains(body, `"ok":false`) {
		t.Fatal(body)
	}
	for _, args := range f.calls {
		if args[0] == "exec" {
			t.Fatal("changed container was mutated")
		}
	}
}

func TestMemberPasswordResetRejectsDeletedIdentity(t *testing.T) {
	h, f, _ := fixture(t)
	id := strings.Repeat("c", 32)
	if _, err := h.db.SQL.Exec("INSERT INTO member_container_slots(member_id,username,plan,deleted) VALUES(?,?,'',1)", id, "bob"); err != nil {
		t.Fatal(err)
	}
	if status, _ := call(h, "POST", "/api/containers/members/"+id+"/password", `{"username":"bob","password":"New-password-123"}`, admin); status == 200 {
		t.Fatal("deleted member reset accepted")
	}
	if len(f.calls) != 0 {
		t.Fatal("deleted member touched Docker")
	}
}
