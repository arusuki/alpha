CREATE TABLE rootless_container_owners (
 container_id TEXT PRIMARY KEY CHECK(length(container_id)=64 AND container_id NOT GLOB '*[^a-f0-9]*'),
 owner_container_id TEXT NOT NULL REFERENCES owners(container_id) ON DELETE CASCADE,
 updated_at REAL NOT NULL,
 actor TEXT NOT NULL,
 CHECK(container_id<>owner_container_id)
);
CREATE INDEX rootless_owner_container ON rootless_container_owners(owner_container_id);
