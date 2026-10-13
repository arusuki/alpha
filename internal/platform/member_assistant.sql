CREATE TABLE member_assistant_sessions (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES members(id) ON DELETE CASCADE,
 title TEXT NOT NULL, data TEXT NOT NULL CHECK(json_valid(data)),
 messages TEXT NOT NULL CHECK(json_valid(messages)),
 created_at REAL NOT NULL, updated_at REAL NOT NULL
);
CREATE INDEX idx_member_assistant_sessions_user ON member_assistant_sessions(user_id,updated_at DESC);
CREATE TABLE member_assistant_memories (
 user_id TEXT PRIMARY KEY REFERENCES members(id) ON DELETE CASCADE,
 data TEXT NOT NULL CHECK(json_valid(data)), updated_at REAL NOT NULL
);
