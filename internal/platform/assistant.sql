CREATE TABLE assistant_sessions (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 title TEXT NOT NULL, data TEXT NOT NULL CHECK(json_valid(data)),
 messages TEXT NOT NULL CHECK(json_valid(messages)),
 created_at REAL NOT NULL, updated_at REAL NOT NULL
);
CREATE INDEX idx_assistant_sessions_user ON assistant_sessions(user_id,updated_at DESC);
CREATE TABLE assistant_memories (
 user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 data TEXT NOT NULL CHECK(json_valid(data)), updated_at REAL NOT NULL
);
