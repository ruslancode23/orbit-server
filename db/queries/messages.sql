-- A resend with the same client id returns the message already stored
-- instead of creating a second one (FR-04).
-- name: CreateMessage :one
INSERT INTO messages (channel_id, author_id, client_message_id, content, reply_to_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (channel_id, author_id, client_message_id) WHERE author_id IS NOT NULL
    DO UPDATE SET channel_id = excluded.channel_id
RETURNING *;

-- name: GetMessage :one
SELECT * FROM messages WHERE id = $1;

-- One page of history, newest first; the caller passes the seq it already has
-- (0 for the first page) and gets the next portion.
-- name: ListChannelMessagesBefore :many
SELECT * FROM messages
WHERE channel_id = $1
  AND (sqlc.narg(before_seq)::bigint IS NULL OR seq < sqlc.narg(before_seq)::bigint)
ORDER BY seq DESC
LIMIT $2;

-- name: EditMessage :one
UPDATE messages
SET content = $3, edited_at = now()
WHERE id = $1 AND author_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: DeleteMessage :one
UPDATE messages
SET deleted_at = now(), content = ''
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: AddMessageMention :exec
INSERT INTO message_mentions (message_id, user_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: ListMessageMentions :many
SELECT user_id FROM message_mentions WHERE message_id = ANY($1::uuid[]);

-- name: AddReaction :one
INSERT INTO message_reactions (message_id, user_id, emoji)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: RemoveReaction :execrows
DELETE FROM message_reactions WHERE message_id = $1 AND user_id = $2 AND emoji = $3;

-- name: ListMessageReactions :many
SELECT message_id, emoji, count(*) AS total,
       bool_or(user_id = sqlc.arg(viewer_id)::uuid) AS reacted
FROM message_reactions
WHERE message_id = ANY(sqlc.arg(message_ids)::uuid[])
GROUP BY message_id, emoji
ORDER BY message_id, emoji;
