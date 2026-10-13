CREATE TABLE chat_cli_settings (
 id INTEGER PRIMARY KEY CHECK(id=1), revision INTEGER NOT NULL,
 server TEXT NOT NULL, command_path TEXT NOT NULL
);
INSERT INTO chat_cli_settings VALUES(1,1,'','/usr/local/bin/alpha-chat');
CREATE TABLE member_cli_sync (
 member_id TEXT NOT NULL REFERENCES members(id) ON DELETE CASCADE,
 node_id TEXT NOT NULL REFERENCES cluster_nodes(id) ON DELETE CASCADE,
 state TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', updated_at REAL NOT NULL,
 generation INTEGER NOT NULL DEFAULT 1,
 PRIMARY KEY(member_id,node_id)
);
