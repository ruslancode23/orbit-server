-- name: CreateWorkspace :one
INSERT INTO workspaces (name, owner_id) VALUES ($1, $2) RETURNING *;

-- name: GetWorkspace :one
SELECT * FROM workspaces WHERE id = $1;

-- name: RenameWorkspace :one
UPDATE workspaces SET name = $2, updated_at = now() WHERE id = $1 RETURNING *;

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

-- name: SetWorkspaceRole :one
UPDATE workspace_members SET role = $3 WHERE workspace_id = $1 AND user_id = $2 RETURNING *;

-- name: RemoveWorkspaceMember :execrows
DELETE FROM workspace_members WHERE workspace_id = $1 AND user_id = $2;

-- name: ListWorkspaceMembers :many
SELECT sqlc.embed(users), workspace_members.role, workspace_members.joined_at
FROM workspace_members
    JOIN users ON users.id = workspace_members.user_id
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
