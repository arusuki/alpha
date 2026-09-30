CREATE TABLE bastion_tailscale (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, enabled INTEGER NOT NULL CHECK(enabled IN (0,1))
);
CREATE TABLE bastion_accounts (
 id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, home TEXT NOT NULL UNIQUE,
 uid INTEGER NOT NULL, gid INTEGER NOT NULL, host TEXT NOT NULL, port INTEGER NOT NULL,
 enabled INTEGER NOT NULL CHECK(enabled IN (0,1))
);
CREATE TABLE member_access (
 member_id TEXT PRIMARY KEY REFERENCES members(id),
 tailscale_id TEXT REFERENCES bastion_tailscale(id), account_id TEXT REFERENCES bastion_accounts(id),
 invite_id TEXT NOT NULL DEFAULT '', invite_url TEXT NOT NULL DEFAULT '',
 invite_state TEXT NOT NULL DEFAULT 'pending', invite_before TEXT NOT NULL DEFAULT '[]',
 accepted_by TEXT NOT NULL DEFAULT '', key_state TEXT NOT NULL DEFAULT 'pending',
 error TEXT NOT NULL DEFAULT '', updated_at REAL NOT NULL
);
CREATE UNIQUE INDEX member_invite_id ON member_access(invite_id) WHERE invite_id<>'';
