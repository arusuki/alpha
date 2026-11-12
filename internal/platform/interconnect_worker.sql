CREATE TABLE link_local (id INTEGER PRIMARY KEY CHECK(id=1), desired INTEGER NOT NULL, applied INTEGER NOT NULL, config TEXT NOT NULL, error TEXT NOT NULL);
INSERT INTO link_local VALUES(1,0,0,'','');
CREATE TABLE link_claims (container TEXT PRIMARY KEY REFERENCES managed_containers(id));
CREATE TABLE link_operations (id INTEGER PRIMARY KEY, revision INTEGER NOT NULL, at REAL NOT NULL, result TEXT NOT NULL);
CREATE TRIGGER link_guard_owner BEFORE UPDATE OF owner ON managed_containers WHEN OLD.owner<>NEW.owner AND EXISTS(SELECT 1 FROM link_claims WHERE container=OLD.id)
 BEGIN SELECT RAISE(ABORT,'请先退出容器互联再更改归属'); END;
CREATE TRIGGER link_guard_release BEFORE DELETE ON managed_containers WHEN EXISTS(SELECT 1 FROM link_claims WHERE container=OLD.id)
 BEGIN SELECT RAISE(ABORT,'请先退出容器互联再解除接管'); END;
