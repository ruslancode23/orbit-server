-- name: CreateInvite :one
INSERT INTO invites (workspace_id, code, created_by, expires_at, max_uses)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- Only a usable invite comes back: expiry, revocation and the use limit are checked here.
-- name: GetLiveInvite :one
SELECT * FROM invites
WHERE code = $1
  AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > now())
  AND (max_uses IS NULL OR uses < max_uses);

-- name: UseInvite :one
UPDATE invites
SET uses = uses + 1
WHERE id = $1
  AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > now())
  AND (max_uses IS NULL OR uses < max_uses)
RETURNING *;

-- name: RevokeInvite :execrows
UPDATE invites SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: ListWorkspaceInvites :many
SELECT * FROM invites WHERE workspace_id = $1 ORDER BY created_at DESC;

-- name: GetInvite :one
SELECT * FROM invites WHERE id = $1;
