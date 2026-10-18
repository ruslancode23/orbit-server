-- A conversation is a channel without a server: the same messages, attachments
-- and read cursors work for it (ADR in migration 00003).
-- name: CreateConversation :one
INSERT INTO channels (kind, direct_key)
VALUES (sqlc.arg(kind), sqlc.narg(direct_key))
RETURNING *;

-- name: AddChannelMember :exec
INSERT INTO channel_members (channel_id, user_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: IsChannelMember :one
SELECT EXISTS (
    SELECT 1 FROM channel_members WHERE channel_id = $1 AND user_id = $2
);

-- name: ListConversationMembers :many
SELECT sqlc.embed(users), channel_members.joined_at
FROM channel_members
    JOIN users ON users.id = channel_members.user_id
WHERE channel_members.channel_id = $1
ORDER BY channel_members.joined_at;

-- name: GetDirectConversation :one
SELECT * FROM channels WHERE direct_key = $1;

-- The conversations of one account, most recently used first, so the list
-- reads like a messenger's.
-- name: ListUserConversations :many
SELECT sqlc.embed(channels), coalesce(max(messages.seq), 0)::bigint AS last_seq
FROM channel_members
    JOIN channels ON channels.id = channel_members.channel_id
    LEFT JOIN messages ON messages.channel_id = channels.id
WHERE channel_members.user_id = $1 AND channels.workspace_id IS NULL
GROUP BY channels.id
ORDER BY last_seq DESC, channels.created_at DESC;

-- name: CountChannelMembers :one
SELECT count(*) FROM channel_members WHERE channel_id = $1;
