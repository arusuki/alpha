CREATE TABLE member_cli_configs (
 username TEXT PRIMARY KEY, member_id TEXT NOT NULL UNIQUE,
 server TEXT NOT NULL, command_path TEXT NOT NULL
);
CREATE TABLE container_cli_installs (
 container_id TEXT PRIMARY KEY REFERENCES managed_containers(id) ON DELETE CASCADE,
 command_path TEXT NOT NULL, directory TEXT NOT NULL
);
