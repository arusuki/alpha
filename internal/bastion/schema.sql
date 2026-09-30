CREATE TABLE bastion_tailscale (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, enabled INTEGER NOT NULL CHECK(enabled IN (0,1))
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
