-- Source: v0.4.0 internal/platform/schema.sql (schema 34).
CREATE TABLE users (
 id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL,
 role TEXT NOT NULL CHECK(role IN ('admin','viewer')), enabled INTEGER NOT NULL DEFAULT 1,
 created_at REAL NOT NULL
);
CREATE TABLE sessions (
 token_hash TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 csrf TEXT NOT NULL, expires_at REAL NOT NULL
);
CREATE INDEX idx_sessions_expiry ON sessions(expires_at);
CREATE TABLE service_identity (
 id INTEGER PRIMARY KEY CHECK(id=1), mode TEXT NOT NULL CHECK(mode IN ('control','worker','registry')),
 instance_id TEXT NOT NULL
);
CREATE TABLE audit (
 id INTEGER PRIMARY KEY AUTOINCREMENT, at REAL NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, detail TEXT NOT NULL
);
