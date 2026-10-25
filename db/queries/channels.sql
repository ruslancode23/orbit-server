-- A new channel goes to the end of the list: position is one past the current last.
-- name: CreateChannel :one
INSERT INTO channels (workspace_id, kind, name, topic, position)
VALUES (
    sqlc.arg(workspace_id),
    sqlc.arg(kind),
    sqlc.arg(name),
    sqlc.narg(topic),
    coalesce(
        (SELECT max(position) + 1 FROM channels WHERE workspace_id = sqlc.arg(workspace_id)),
        0
    )
)
RETURNING *;

-- name: GetChannel :one
SELECT * FROM channels WHERE id = $1;

-- name: ListWorkspaceChannels :many
SELECT * FROM channels
WHERE workspace_id = $1
ORDER BY position, created_at;

-- name: UpdateChannel :one
UPDATE channels
SET name = coalesce(sqlc.narg(name), name),
    topic = CASE WHEN sqlc.arg(clear_topic)::boolean THEN NULL ELSE coalesce(sqlc.narg(topic), topic) END,
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- Reordering sends the whole list: position is the index of the channel in it.
-- name: SetChannelPosition :execrows
UPDATE channels
SET position = sqlc.arg(position), updated_at = now()
WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id);

-- name: DeleteChannel :exec
DELETE FROM channels WHERE id = $1;

-- name: GetChannelsByIDs :many
-- Names and servers of a page of channels, for the search results.
SELECT * FROM channels WHERE id = ANY(sqlc.arg(ids)::uuid[]);
