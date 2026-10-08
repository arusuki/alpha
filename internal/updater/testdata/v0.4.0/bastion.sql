-- Source: v0.4.0 internal/bastion/schema.sql (schema 34).
CREATE TABLE bastion_ssh_settings (
 id INTEGER PRIMARY KEY CHECK(id=1), identity_file TEXT NOT NULL DEFAULT '',
 revision INTEGER NOT NULL DEFAULT 1 CHECK(revision>0)
);
INSERT INTO bastion_ssh_settings(id) VALUES(1);
CREATE TABLE bastion_tailscale (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
 ssh_host TEXT NOT NULL, ssh_port INTEGER NOT NULL CHECK(ssh_port BETWEEN 1 AND 65535),
 status_port INTEGER NOT NULL CHECK(status_port BETWEEN 1024 AND 65535),
 control_url TEXT NOT NULL,
 UNIQUE(ssh_host,status_port)
);
CREATE TABLE member_access (
 member_id TEXT PRIMARY KEY REFERENCES members(id),
 tailscale_id TEXT REFERENCES bastion_tailscale(id),
 invite_id TEXT NOT NULL DEFAULT '', invite_url TEXT NOT NULL DEFAULT '',
 invite_state TEXT NOT NULL DEFAULT 'pending', invite_before TEXT NOT NULL DEFAULT '[]',
 accepted_by TEXT NOT NULL DEFAULT '', key_state TEXT NOT NULL DEFAULT 'pending',
 error TEXT NOT NULL DEFAULT '', updated_at REAL NOT NULL
);
CREATE UNIQUE INDEX member_invite_id ON member_access(invite_id) WHERE invite_id<>'';
