-- Source: v0.5.0 internal/platform/gpu_schema.go (schema 36).
CREATE TABLE gpu_intervals (
 gpu_uuid TEXT NOT NULL, name TEXT NOT NULL, started_at REAL NOT NULL,
 ended_at REAL NOT NULL CHECK(ended_at>started_at), utilization REAL,
 owners TEXT NOT NULL, PRIMARY KEY(gpu_uuid,ended_at));
 CREATE INDEX gpu_intervals_expiry ON gpu_intervals(ended_at);
