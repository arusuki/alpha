CREATE TABLE agent_settings (
 id INTEGER PRIMARY KEY CHECK(id=1), value TEXT NOT NULL,
 api_key_ciphertext TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE agent_sessions (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), title TEXT NOT NULL,
 status TEXT NOT NULL, created_at REAL NOT NULL, updated_at REAL NOT NULL,
 snapshot_id TEXT, active_job_id TEXT, error TEXT, provider TEXT NOT NULL, model TEXT NOT NULL
);
CREATE INDEX idx_agent_sessions_user ON agent_sessions(user_id,updated_at DESC);
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
 session_id TEXT PRIMARY KEY REFERENCES agent_sessions(id) ON DELETE CASCADE,
 report_id INTEGER NOT NULL UNIQUE REFERENCES agent_reports(message_id),
 phase TEXT NOT NULL DEFAULT 'extract'
);
CREATE TABLE agent_cleanup_entries (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES agent_cleanups(session_id) ON DELETE CASCADE,
 path TEXT NOT NULL, category INTEGER NOT NULL, summary TEXT NOT NULL, detail TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending', error TEXT NOT NULL DEFAULT '',
 UNIQUE(session_id,path)
);
-- Keep record references consistent in the same transaction as record deletion.
CREATE TRIGGER agent_record_delete_guard BEFORE DELETE ON jobs
WHEN EXISTS (SELECT 1 FROM agent_sessions WHERE status IN ('queued','scanning','running','cancelling') AND (snapshot_id=OLD.id OR active_job_id=OLD.id))
BEGIN SELECT RAISE(ABORT, 'record is in use by an active analysis'); END;
CREATE TRIGGER agent_record_deleted AFTER DELETE ON jobs BEGIN
 UPDATE agent_sessions SET snapshot_id=NULL WHERE snapshot_id=OLD.id;
 UPDATE agent_sessions SET active_job_id=NULL WHERE active_job_id=OLD.id;
END;
