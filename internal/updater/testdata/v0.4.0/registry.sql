-- Source: v0.4.0 internal/registry/schema.sql (schema 34).
CREATE TABLE registry_control (
 id INTEGER PRIMARY KEY CHECK(id=1), control_id TEXT NOT NULL
);
CREATE TABLE registry_sessions (
 token_hash TEXT PRIMARY KEY, invitation_hash TEXT NOT NULL, csrf TEXT NOT NULL,
 resource_token TEXT NOT NULL, schema_json TEXT NOT NULL, registration TEXT NOT NULL DEFAULT '',
 registered INTEGER NOT NULL DEFAULT 0 CHECK(registered IN (0,1)), expires_at REAL NOT NULL
);
