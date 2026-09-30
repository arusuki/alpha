CREATE TABLE container_settings (id INTEGER PRIMARY KEY CHECK(id=1), value TEXT NOT NULL);
CREATE TABLE managed_containers (
 id TEXT PRIMARY KEY, endpoint TEXT NOT NULL, daemon TEXT NOT NULL, name TEXT NOT NULL,
 owner TEXT NOT NULL, spec TEXT NOT NULL, fingerprint TEXT NOT NULL, origin TEXT NOT NULL,
 gate TEXT NOT NULL, initialized INTEGER NOT NULL, created_at REAL NOT NULL,
 UNIQUE(endpoint,daemon,name));
