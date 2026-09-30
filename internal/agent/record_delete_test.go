package agent

import (
	"testing"

	"project-alpha/internal/platform"
)

func TestSessionNodeIsolation(t *testing.T) {
	p := newTestPlatform(t)
	p.login(true, "admin", "administrator-password")
	var uid string
	if err := p.db.SQL.QueryRow("SELECT id FROM users WHERE username='admin'").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	session := platform.RandomHex(16)
	if _, err := p.db.SQL.Exec("INSERT INTO agent_sessions(id,user_id,node_id,title,status,created_at,updated_at,provider,model) VALUES(?,?,'another-node','analysis','completed',0,0,'test','test')", session, uid); err != nil {
		t.Fatal(err)
	}
	p.expect(404, "GET", "/api/agent/sessions/"+session, nil, nil)
	p.expect(404, "POST", "/api/agent/sessions/"+session+"/cancel", object{}, nil)
	result := p.expect(200, "GET", "/api/agent/sessions", nil, nil)
	if len(result["sessions"].([]any)) != 0 {
		t.Fatal("another node's session leaked")
	}
}
