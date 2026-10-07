-- Source: v0.3.1 internal/cluster/{store,provision}.go (schema 33).
CREATE TABLE cluster_nodes (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, url TEXT NOT NULL UNIQUE,
 token TEXT NOT NULL, created_at REAL NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('worker','registry')),
 internal_ip TEXT NOT NULL, CHECK((kind='worker' AND length(internal_ip)>0) OR (kind='registry' AND internal_ip=''))
 );
CREATE TABLE member_node_resources (
 member_id TEXT NOT NULL REFERENCES members(id), node_id TEXT NOT NULL REFERENCES cluster_nodes(id),
 state TEXT NOT NULL, container_id TEXT NOT NULL DEFAULT '',name TEXT NOT NULL DEFAULT '',port INTEGER NOT NULL DEFAULT 0,
 error TEXT NOT NULL DEFAULT '', updated_at REAL NOT NULL,
 PRIMARY KEY(member_id,node_id));
 CREATE TABLE member_work (member_id TEXT PRIMARY KEY REFERENCES members(id),pending INTEGER NOT NULL);
