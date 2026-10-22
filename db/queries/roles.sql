-- Roles of a server (internal/servers) and their overrides in channels
-- (internal/channels). The permission bits are those of internal/perms.

-- name: CreateRole :one
INSERT INTO server_roles (workspace_id, name, color, hoist, position, permissions, is_default)
VALUES (
    sqlc.arg(workspace_id),
    sqlc.arg(name),
    sqlc.narg(color),
    sqlc.arg(hoist),
    sqlc.arg(position),
    sqlc.arg(permissions),
    sqlc.arg(is_default)
)
RETURNING *;

-- name: ListWorkspaceRoles :many
SELECT * FROM server_roles
WHERE workspace_id = $1
ORDER BY position DESC;

-- The roles one member holds; @everyone is implied and not listed.
-- name: ListMemberRoleIDs :many
SELECT role_id FROM server_member_roles
WHERE workspace_id = $1 AND user_id = $2;

-- The roles of every member of a server, for a member list in one query.
-- name: ListWorkspaceMemberRoles :many
SELECT user_id, role_id FROM server_member_roles
WHERE workspace_id = $1;

-- name: SetDefaultRolePermissions :exec
UPDATE server_roles
SET permissions = sqlc.arg(permissions)
WHERE workspace_id = sqlc.arg(workspace_id) AND is_default;

-- name: GrantRole :exec
INSERT INTO server_member_roles (workspace_id, user_id, role_id)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- Takes away every role of the member that carries any of these bits.
-- name: RevokeRolesWithPermissions :execrows
DELETE FROM server_member_roles held
USING server_roles roles
WHERE held.role_id = roles.id
  AND held.workspace_id = sqlc.arg(workspace_id)
  AND held.user_id = sqlc.arg(user_id)
  AND roles.permissions & sqlc.arg(bits)::bigint <> 0;

-- The most senior role position of a server, to put a new role above it.
-- name: MaxRolePosition :one
SELECT coalesce(max(position), 0)::integer FROM server_roles WHERE workspace_id = $1;

-- name: ListChannelOverrides :many
SELECT role_id, allow, deny FROM channel_role_overrides WHERE channel_id = $1;

-- The overrides of every channel of a server, for a channel list in one query.
-- name: ListWorkspaceOverrides :many
SELECT overrides.channel_id, overrides.role_id, overrides.allow, overrides.deny
FROM channel_role_overrides overrides
    JOIN channels ON channels.id = overrides.channel_id
WHERE channels.workspace_id = $1;

-- name: SetChannelOverride :exec
INSERT INTO channel_role_overrides (channel_id, role_id, allow, deny)
VALUES ($1, $2, $3, $4)
ON CONFLICT (channel_id, role_id) DO UPDATE SET allow = excluded.allow, deny = excluded.deny;

-- name: DeleteChannelOverride :exec
DELETE FROM channel_role_overrides WHERE channel_id = $1 AND role_id = $2;

-- name: GetDefaultRole :one
SELECT * FROM server_roles WHERE workspace_id = $1 AND is_default;
