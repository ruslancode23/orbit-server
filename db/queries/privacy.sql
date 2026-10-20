-- Who may write to whom (SCRUM-85). The privacy module owns users.dm_policy and
-- user_blocks; the users table itself belongs to internal/auth, which is why
-- only this module reads the policy column.

-- Both accounts of a dialogue in one query: a missing row means an unknown
-- account, which the caller treats the same as a refusal.
-- name: GetUserPrivacy :many
SELECT id, dm_policy, friend_request_policy, is_guest
FROM users
WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- The only proof of acquaintance the product has so far: two accounts on one
-- server. One query, so starting a dialogue does not walk a list of servers.
-- name: HasSharedWorkspace :one
SELECT EXISTS (
    SELECT 1
    FROM workspace_members mine
        JOIN workspace_members theirs ON theirs.workspace_id = mine.workspace_id
    WHERE mine.user_id = sqlc.arg(a)::uuid
      AND theirs.user_id = sqlc.arg(b)::uuid
);

-- A block counts in both directions: the blocked person cannot start either.
-- name: IsBlockedEitherWay :one
SELECT EXISTS (
    SELECT 1 FROM user_blocks
    WHERE (blocker_id = sqlc.arg(a)::uuid AND blocked_id = sqlc.arg(b)::uuid)
       OR (blocker_id = sqlc.arg(b)::uuid AND blocked_id = sqlc.arg(a)::uuid)
);

-- The accounts this person blocked: their authors are hidden in history and in
-- live group events.
-- name: ListBlockedIDs :many
SELECT blocked_id FROM user_blocks WHERE blocker_id = $1;

-- The settings screen: whom the caller blocked and when.
-- name: ListBlockedUsers :many
SELECT users.id, users.nickname, user_blocks.created_at
FROM user_blocks
    JOIN users ON users.id = user_blocks.blocked_id
WHERE user_blocks.blocker_id = $1
ORDER BY user_blocks.created_at DESC;

-- Blocking twice is the same as blocking once: the endpoint is idempotent.
-- name: BlockUser :exec
INSERT INTO user_blocks (blocker_id, blocked_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: UnblockUser :exec
DELETE FROM user_blocks WHERE blocker_id = $1 AND blocked_id = $2;

-- Blocking an id that names no account is a 404, not a foreign key error.
-- name: UserExists :one
SELECT EXISTS (SELECT 1 FROM users WHERE id = $1);

-- name: GetDMPolicy :one
SELECT dm_policy FROM users WHERE id = $1;

-- name: SetDMPolicy :exec
UPDATE users SET dm_policy = $2, updated_at = now() WHERE id = $1;

-- name: SetFriendRequestPolicy :exec
UPDATE users SET friend_request_policy = $2, updated_at = now() WHERE id = $1;
