CREATE TABLE link_settings (id INTEGER PRIMARY KEY CHECK(id=1), pool TEXT NOT NULL, revision INTEGER NOT NULL);
INSERT INTO link_settings VALUES(1,'10.240.0.0/16',0);
CREATE TABLE link_nodes (node TEXT PRIMARY KEY REFERENCES cluster_nodes(id) ON DELETE CASCADE,
 subnet TEXT NOT NULL UNIQUE, desired INTEGER NOT NULL, applied INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '');
CREATE TABLE link_endpoints (node TEXT NOT NULL REFERENCES link_nodes(node), container TEXT NOT NULL, ip TEXT UNIQUE NOT NULL,
 name TEXT NOT NULL, owner TEXT NOT NULL, removing INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(node,container));
CREATE TABLE link_connections (id TEXT PRIMARY KEY, config TEXT NOT NULL, revision INTEGER NOT NULL, removing INTEGER NOT NULL DEFAULT 0);
CREATE TRIGGER link_guard_node_ip BEFORE UPDATE OF internal_ip ON cluster_nodes
 WHEN OLD.internal_ip<>NEW.internal_ip AND EXISTS(SELECT 1 FROM link_endpoints WHERE node=OLD.id)
 BEGIN SELECT RAISE(ABORT,'请先退出节点上的互联端点'); END;
CREATE TRIGGER link_guard_node_delete BEFORE DELETE ON cluster_nodes WHEN EXISTS(SELECT 1 FROM link_endpoints WHERE node=OLD.id)
 BEGIN SELECT RAISE(ABORT,'请先退出节点上的互联端点'); END;
