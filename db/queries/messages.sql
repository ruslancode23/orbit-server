-- A resend with the same client id returns the message already stored
-- instead of creating a second one (FR-04).
-- name: CreateMessage :one
INSERT INTO messages (channel_id, author_id, client_message_id, content, reply_to_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (channel_id, author_id, client_message_id) WHERE author_id IS NOT NULL
    DO UPDATE SET channel_id = excluded.channel_id
-- xmax = 0 on a row this statement inserted; anything else means the resend
-- found the message already stored.
RETURNING *, (xmax = 0) AS inserted;

-- name: GetMessage :one
SELECT sqlc.embed(messages), sqlc.embed(users)
FROM messages
    LEFT JOIN users ON users.id = messages.author_id
WHERE messages.id = $1;

-- One page of history, newest first: the caller passes the seq it already has
-- (NULL for the first page) and gets the next portion.
-- name: ListChannelMessages :many
SELECT sqlc.embed(messages), sqlc.embed(users)
FROM messages
    LEFT JOIN users ON users.id = messages.author_id
WHERE messages.channel_id = $1
  AND (sqlc.narg(before_seq)::bigint IS NULL OR messages.seq < sqlc.narg(before_seq)::bigint)
ORDER BY messages.seq DESC
LIMIT $2;

-- name: EditMessage :one
UPDATE messages
SET content = $3, edited_at = now()
WHERE id = $1 AND author_id = $2 AND deleted_at IS NULL
RETURNING *;

-- Soft delete: replies and read cursors keep pointing at the message.
-- name: DeleteMessage :one
UPDATE messages
SET deleted_at = now(), content = ''
WHERE id = $1 AND author_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: AddMessageMention :exec
INSERT INTO message_mentions (message_id, user_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: ListMessageMentions :many
SELECT message_id, user_id FROM message_mentions
WHERE message_id = ANY($1::uuid[]);

-- name: AddReaction :exec
INSERT INTO message_reactions (message_id, user_id, emoji)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: RemoveReaction :execrows
DELETE FROM message_reactions WHERE message_id = $1 AND user_id = $2 AND emoji = $3;

-- Reactions of a page of messages, with whether the caller is among them.
-- name: ListMessageReactions :many
SELECT message_id, emoji, count(*) AS total,
       bool_or(user_id = sqlc.arg(viewer_id)::uuid) AS reacted
FROM message_reactions
WHERE message_id = ANY(sqlc.arg(message_ids)::uuid[])
GROUP BY message_id, emoji
ORDER BY message_id, emoji;

-- What a reconnecting client missed in one channel, oldest first (task 2.6).
-- name: ListChannelMessagesAfter :many
SELECT sqlc.embed(messages), sqlc.embed(users)
FROM messages
    LEFT JOIN users ON users.id = messages.author_id
WHERE messages.channel_id = $1 AND messages.seq > sqlc.arg(after_seq)
ORDER BY messages.seq
LIMIT $2;

-- The first message of a dialogue spends the same per-account rate-limit token
-- as opening one; this answers whether the dialogue has any message yet.
-- name: ChannelHasMessages :one
SELECT EXISTS (SELECT 1 FROM messages WHERE channel_id = $1);
