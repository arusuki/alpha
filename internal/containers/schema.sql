CREATE TABLE container_settings (id INTEGER PRIMARY KEY CHECK(id=1), value TEXT NOT NULL);
CREATE TABLE managed_containers (
 id TEXT PRIMARY KEY, endpoint TEXT NOT NULL, daemon TEXT NOT NULL, name TEXT NOT NULL,
 owner TEXT NOT NULL, spec TEXT NOT NULL, fingerprint TEXT NOT NULL, origin TEXT NOT NULL,
 gate TEXT NOT NULL, initialized INTEGER NOT NULL, created_at REAL NOT NULL,
 UNIQUE(endpoint,daemon,name));

CREATE TABLE member_container_slots (member_id TEXT PRIMARY KEY, username TEXT NOT NULL, plan TEXT NOT NULL, mode TEXT NOT NULL DEFAULT 'create' CHECK(mode IN ('create','adopt')), container_id TEXT NOT NULL DEFAULT '', deleted INTEGER NOT NULL CHECK(deleted IN (0,1)));
CREATE UNIQUE INDEX member_slot_owner ON member_container_slots(username) WHERE deleted=0;

CREATE UNIQUE INDEX member_slot_container ON member_container_slots(container_id) WHERE deleted=0 AND container_id<>'';
CREATE UNIQUE INDEX managed_container_owner ON managed_containers(owner) WHERE owner<>'';
