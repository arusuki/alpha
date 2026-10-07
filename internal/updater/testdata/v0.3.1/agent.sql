-- Source: v0.3.1 internal/agent/schema.sql (schema 33).
CREATE TABLE agent_settings (
 id INTEGER PRIMARY KEY CHECK(id=1), value TEXT NOT NULL,
 api_key_ciphertext TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE agent_sessions (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), node_id TEXT NOT NULL DEFAULT '', title TEXT NOT NULL,
 status TEXT NOT NULL, created_at REAL NOT NULL, updated_at REAL NOT NULL,
 snapshot_id TEXT, active_job_id TEXT, error TEXT, provider TEXT NOT NULL, model TEXT NOT NULL,
 report_scope TEXT NOT NULL DEFAULT '' CHECK(report_scope IN ('','host','container'))
);
CREATE INDEX idx_agent_sessions_user ON agent_sessions(node_id,user_id,updated_at DESC);
CREATE TABLE agent_messages (
 id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
 role TEXT NOT NULL, content TEXT NOT NULL, tool_name TEXT, created_at REAL NOT NULL
);
CREATE INDEX idx_agent_messages_session ON agent_messages(session_id,id);
CREATE TABLE agent_reports (
 message_id INTEGER PRIMARY KEY REFERENCES agent_messages(id) ON DELETE CASCADE,
 entries TEXT NOT NULL
);
CREATE TABLE agent_cleanups (
 id TEXT PRIMARY KEY, report_id INTEGER NOT NULL UNIQUE REFERENCES agent_reports(message_id) ON DELETE CASCADE,
 status TEXT NOT NULL DEFAULT 'ready', error TEXT NOT NULL DEFAULT '',
 created_at REAL NOT NULL, updated_at REAL NOT NULL
);
CREATE TABLE agent_cleanup_entries (
 id TEXT PRIMARY KEY, cleanup_id TEXT NOT NULL REFERENCES agent_cleanups(id) ON DELETE CASCADE,
 path TEXT NOT NULL, category INTEGER NOT NULL, summary TEXT NOT NULL, detail TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending', error TEXT NOT NULL DEFAULT '',
 UNIQUE(cleanup_id,path)
);
