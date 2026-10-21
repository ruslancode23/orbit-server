-- name: CreateWorkspace :one
INSERT INTO workspaces (
    name, description, owner_id,
    members_can_invite, members_can_create_channels, members_can_attach_files
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetWorkspace :one
SELECT * FROM workspaces WHERE id = $1;

-- Only what the caller sent changes: a NULL name keeps the old one, and the
-- description is touched only when set_description is true (NULL then clears it).
-- name: UpdateWorkspace :one
UPDATE workspaces
SET name                        = coalesce(sqlc.narg(name), name),
    description                 = CASE WHEN sqlc.arg(set_description)::bool THEN sqlc.narg(description) ELSE description END,
    members_can_invite          = coalesce(sqlc.narg(members_can_invite), members_can_invite),
    members_can_create_channels = coalesce(sqlc.narg(members_can_create_channels), members_can_create_channels),
    members_can_attach_files    = coalesce(sqlc.narg(members_can_attach_files), members_can_attach_files),
    updated_at                  = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- Swaps the icon and reports the key it replaced, so the caller can drop the old
-- object. The row lock keeps two uploads at once from losing track of one.
-- name: SetWorkspaceIcon :one
WITH previous AS (
    SELECT id, icon_key FROM workspaces WHERE workspaces.id = sqlc.arg(id) FOR UPDATE
)
UPDATE workspaces
SET icon_key = sqlc.narg(icon_key), updated_at = now()
FROM previous
WHERE workspaces.id = previous.id
RETURNING workspaces.id, previous.icon_key AS previous_icon_key;

-- name: DeleteWorkspace :exec
DELETE FROM workspaces WHERE id = $1;

-- name: ListUserWorkspaces :many
SELECT sqlc.embed(workspaces), workspace_members.role
FROM workspaces
    JOIN workspace_members ON workspace_members.workspace_id = workspaces.id
WHERE workspace_members.user_id = $1
ORDER BY workspaces.created_at;

-- name: AddWorkspaceMember :one
INSERT INTO workspace_members (workspace_id, user_id, role)
VALUES ($1, $2, $3)
ON CONFLICT (workspace_id, user_id) DO UPDATE SET role = excluded.role
RETURNING *;

-- Every handler asks this before it acts: no row means no access at all.
-- name: GetWorkspaceRole :one
SELECT role FROM workspace_members WHERE workspace_id = $1 AND user_id = $2;

-- The role and what the server lets members do, in one read, for actions a
-- server may open to members.
-- name: GetWorkspaceAccess :one
SELECT workspace_members.role,
       workspaces.members_can_invite,
       workspaces.members_can_create_channels,
       workspaces.members_can_attach_files
FROM workspace_members
    JOIN workspaces ON workspaces.id = workspace_members.workspace_id
WHERE workspace_members.workspace_id = $1 AND workspace_members.user_id = $2;

-- name: SetWorkspaceRole :one
UPDATE workspace_members SET role = $3 WHERE workspace_id = $1 AND user_id = $2 RETURNING *;

-- name: RemoveWorkspaceMember :execrows
DELETE FROM workspace_members WHERE workspace_id = $1 AND user_id = $2;

-- The member list carries ids only: internal/profile fills in the public part
-- of every account in one query, so this module never reads auth's table.
-- name: ListWorkspaceMembers :many
SELECT workspace_members.user_id, workspace_members.role, workspace_members.joined_at
FROM workspace_members
WHERE workspace_members.workspace_id = $1
ORDER BY workspace_members.joined_at;

-- name: BanWorkspaceMember :one
INSERT INTO bans (workspace_id, user_id, banned_by, reason)
VALUES ($1, $2, $3, $4)
ON CONFLICT (workspace_id, user_id) DO UPDATE SET reason = excluded.reason
RETURNING *;

-- name: UnbanWorkspaceMember :execrows
DELETE FROM bans WHERE workspace_id = $1 AND user_id = $2;

-- name: IsWorkspaceMemberBanned :one
SELECT EXISTS (SELECT 1 FROM bans WHERE workspace_id = $1 AND user_id = $2);

-- Same as the member list: ids only, the public part comes from internal/profile.
-- name: ListWorkspaceBans :many
SELECT bans.user_id, bans.reason, bans.banned_by, bans.created_at
FROM bans
WHERE bans.workspace_id = $1
ORDER BY bans.created_at DESC;

-- A server always keeps exactly one owner; the count guards the last one.
-- name: CountWorkspaceOwners :one
SELECT count(*) FROM workspace_members WHERE workspace_id = $1 AND role = 'owner';

-- The mutual lists ask which servers both accounts are on, in one query: reading
-- each membership separately would be an N+1. The role is the caller's own, so
-- the endpoint never shows what the other account may do. The icon key stays in
-- this module; the service turns it into a signed link.
-- name: ListMutualWorkspaces :many
SELECT w.id, w.name, w.icon_key, mine.role
FROM workspace_members mine
    JOIN workspace_members theirs
        ON theirs.workspace_id = mine.workspace_id
        AND theirs.user_id = sqlc.arg(other_id)::uuid
    JOIN workspaces w ON w.id = mine.workspace_id
WHERE mine.user_id = sqlc.arg(user_id)::uuid
ORDER BY lower(w.name), w.id;
