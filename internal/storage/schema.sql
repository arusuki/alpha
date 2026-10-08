CREATE TABLE settings (id INTEGER PRIMARY KEY CHECK(id=1), value TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 1, schedule_last_run REAL NOT NULL DEFAULT 0);
CREATE TABLE jobs (
 id TEXT PRIMARY KEY, status TEXT NOT NULL, trigger TEXT NOT NULL, created_by TEXT NOT NULL,
 created_at REAL NOT NULL, started_at REAL, finished_at REAL, config TEXT NOT NULL,
 progress TEXT NOT NULL DEFAULT '{}', error TEXT, allocated INTEGER, files INTEGER, warnings INTEGER
);
CREATE INDEX idx_jobs_created ON jobs(created_at DESC);
CREATE UNIQUE INDEX idx_jobs_active ON jobs((1)) WHERE status IN ('queued','running','cancelling');
CREATE TABLE owners (container_id TEXT PRIMARY KEY, owner TEXT NOT NULL);
CREATE UNIQUE INDEX owners_one_container ON owners(owner) WHERE owner<>'';
CREATE TABLE snapshot_records (
 job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL,
 root_path TEXT NOT NULL,
 metadata TEXT NOT NULL
);
CREATE TABLE snapshot_nodes (
 job_id TEXT NOT NULL REFERENCES snapshot_records(job_id) ON DELETE CASCADE,
 path TEXT NOT NULL,
 parent TEXT NOT NULL,
 position INTEGER NOT NULL,
 value TEXT NOT NULL,
 identity TEXT,
 PRIMARY KEY(job_id,path)
);
CREATE INDEX snapshot_node_parents ON snapshot_nodes(job_id,parent);
CREATE TABLE snapshot_changes (
 job_id TEXT NOT NULL REFERENCES snapshot_records(job_id) ON DELETE CASCADE,
 path TEXT NOT NULL,
 revision INTEGER NOT NULL,
 PRIMARY KEY(job_id,path)
);
CREATE INDEX idx_jobs_latest ON jobs(finished_at DESC) WHERE status='completed' AND trigger<>'incremental';
CREATE INDEX idx_snapshot_changes_revision ON snapshot_changes(job_id,revision);
