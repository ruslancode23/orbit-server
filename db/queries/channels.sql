-- name: CreateChannel :one
INSERT INTO channels (workspace_id, kind, name, topic, position)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetChannel :one
SELECT * FROM channels WHERE id = $1;

-- name: ListWorkspaceChannels :many
SELECT * FROM channels
WHERE workspace_id = $1
ORDER BY position, created_at;

-- name: UpdateChannel :one
UPDATE channels
SET name = $2, topic = $3, position = $4, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteChannel :exec
DELETE FROM channels WHERE id = $1;
