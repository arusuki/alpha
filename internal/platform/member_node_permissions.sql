CREATE TABLE member_node_permissions (
 member_id TEXT PRIMARY KEY REFERENCES members(id) ON DELETE CASCADE,
 permissions TEXT NOT NULL CHECK(json_valid(permissions))
);
CREATE TABLE invitation_node_permissions (
 invitation_id TEXT PRIMARY KEY REFERENCES member_invitations(id) ON DELETE CASCADE,
 permissions TEXT NOT NULL CHECK(json_valid(permissions))
);
