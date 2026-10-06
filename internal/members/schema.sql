CREATE TABLE member_registration_schema (
 id INTEGER PRIMARY KEY CHECK(id=1), revision INTEGER NOT NULL, fields TEXT NOT NULL
);
INSERT INTO member_registration_schema VALUES(1,1,'[]');
CREATE TABLE member_invitations (
 id TEXT PRIMARY KEY, code_hash TEXT NOT NULL UNIQUE, code_ciphertext TEXT NOT NULL, label TEXT NOT NULL,
 quota INTEGER NOT NULL CHECK(quota BETWEEN 1 AND 100000),
 used INTEGER NOT NULL DEFAULT 0 CHECK(used>=0 AND used<=quota),
 revoked INTEGER NOT NULL DEFAULT 0 CHECK(revoked IN (0,1)),
 created_by TEXT NOT NULL, created_at REAL NOT NULL
);
CREATE TABLE members (
 id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE,
 profile TEXT NOT NULL, registration_schema TEXT NOT NULL,
 password_hash TEXT NOT NULL, password_ciphertext TEXT NOT NULL,
 ssh_public_key TEXT NOT NULL, resource_token_hash TEXT NOT NULL UNIQUE, status TEXT NOT NULL CHECK(status IN ('active','deleting')),
 -- Registration provenance survives invitation deletion and binds registry retries.
 invitation_id TEXT NOT NULL, invitation_code_hash TEXT NOT NULL, created_at REAL NOT NULL
);
CREATE TABLE member_sessions (
 token_hash TEXT PRIMARY KEY, member_id TEXT NOT NULL REFERENCES members(id) ON DELETE CASCADE,
 expires_at REAL NOT NULL
);
CREATE INDEX member_sessions_member ON member_sessions(member_id);
