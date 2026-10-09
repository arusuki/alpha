-- Source: v0.6.0 internal/platform/mihomo.sql (schema 38).
CREATE TABLE mihomo_profiles (
 target TEXT PRIMARY KEY, revision INTEGER NOT NULL, config TEXT NOT NULL,
 updated_at REAL NOT NULL
);
CREATE TABLE mihomo_sync (
 target TEXT PRIMARY KEY, source_version TEXT NOT NULL DEFAULT '',
 bundle TEXT NOT NULL DEFAULT '', refreshed_at REAL NOT NULL DEFAULT 0,
 attempted_at REAL NOT NULL DEFAULT 0, delivered INTEGER NOT NULL DEFAULT 0,
 error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE mihomo_runtime (
 id INTEGER PRIMARY KEY CHECK(id=1), bundle TEXT NOT NULL DEFAULT '',
 enabled INTEGER NOT NULL DEFAULT 0, selections TEXT NOT NULL DEFAULT '{}'
);
INSERT INTO mihomo_runtime(id) VALUES(1);
