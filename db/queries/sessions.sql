-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- Only a live session resolves to its user; expiry and revocation are checked here,
-- not by the caller.
-- name: GetLiveSessionUser :one
SELECT sqlc.embed(users), sessions.id AS session_id, sessions.expires_at
FROM sessions
    JOIN users ON users.id = sessions.user_id
WHERE sessions.token_hash = $1
  AND sessions.revoked_at IS NULL
  AND sessions.expires_at > now();

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = now() WHERE id = $1;

-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeUserSessions :exec
UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at <= now() OR revoked_at IS NOT NULL;
