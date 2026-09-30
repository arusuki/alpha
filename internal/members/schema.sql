CREATE TABLE member_registration_schema (
 id INTEGER PRIMARY KEY CHECK(id=1), revision INTEGER NOT NULL, fields TEXT NOT NULL
);
INSERT INTO member_registration_schema VALUES(1,1,'[]');
CREATE TABLE member_invitations (
 id TEXT PRIMARY KEY, code_hash TEXT NOT NULL UNIQUE, label TEXT NOT NULL,
 quota INTEGER NOT NULL CHECK(quota BETWEEN 1 AND 100000),
 used INTEGER NOT NULL DEFAULT 0 CHECK(used>=0 AND used<=quota),
 revoked INTEGER NOT NULL DEFAULT 0 CHECK(revoked IN (0,1)),
 created_by TEXT NOT NULL, created_at REAL NOT NULL
);
CREATE TABLE members (
 id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE,
 profile TEXT NOT NULL, registration_schema TEXT NOT NULL,
 invitation_id TEXT NOT NULL REFERENCES member_invitations(id), created_at REAL NOT NULL
);
