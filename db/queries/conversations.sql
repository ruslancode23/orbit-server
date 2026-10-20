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

-- The participant list carries ids only: internal/profile fills in the public
-- part in one query, so this module never reads auth's table.
-- name: ListConversationMembers :many
SELECT channel_members.user_id, channel_members.joined_at
FROM channel_members
WHERE channel_members.channel_id = $1
ORDER BY channel_members.joined_at;

-- name: GetDirectConversation :one
SELECT * FROM channels WHERE direct_key = $1 AND kind = 'direct';

-- The conversations of one account, most recently used first, so the list
-- reads like a messenger's.
-- name: ListUserConversations :many
SELECT sqlc.embed(channels), coalesce(max(messages.seq), 0)::bigint AS last_seq
FROM channel_members
    JOIN channels ON channels.id = channel_members.channel_id
    LEFT JOIN messages ON messages.channel_id = channels.id
WHERE channel_members.user_id = $1
  AND channels.workspace_id IS NULL
  -- A secret chat is not a dialogue in the ordinary sense: it lives on one
  -- device and has its own endpoints, so the conversation list leaves it out.
  AND channels.kind IN ('direct', 'group')
GROUP BY channels.id
ORDER BY last_seq DESC, channels.created_at DESC;

-- name: CountChannelMembers :one
SELECT count(*) FROM channel_members WHERE channel_id = $1;

-- Who belongs to a conversation. Messages publish to each participant's personal
-- stream, because a conversation has no server to announce to.
-- name: ListConversationMemberIDs :many
SELECT user_id FROM channel_members WHERE channel_id = $1 ORDER BY joined_at;

-- name: GetSecretConversation :one
SELECT * FROM channels WHERE direct_key = $1 AND kind = 'secret';

-- One query twice, so profile visibility never walks a list of conversations:
-- whether two accounts share any conversation, secret chats included — their
-- participants already know each other.
-- name: HasSharedConversation :one
SELECT EXISTS (
    SELECT 1
    FROM channel_members mine
        JOIN channel_members theirs ON theirs.channel_id = mine.channel_id
        JOIN channels ON channels.id = mine.channel_id
    WHERE mine.user_id = sqlc.arg(a)::uuid
      AND theirs.user_id = sqlc.arg(b)::uuid
      AND channels.workspace_id IS NULL
      AND channels.kind IN ('direct', 'group', 'secret')
);

-- Everyone who shares a conversation with this account: the audience of a
-- user.updated event. A secret chat counts too: the peer already knows the
-- account, and the event travels on the personal stream, not in the chat.
-- name: ListConversationPeers :many
SELECT DISTINCT theirs.user_id
FROM channel_members mine
    JOIN channel_members theirs ON theirs.channel_id = mine.channel_id
    JOIN channels ON channels.id = mine.channel_id
WHERE mine.user_id = $1
  AND theirs.user_id <> $1
  AND channels.workspace_id IS NULL
  AND channels.kind IN ('direct', 'group', 'secret');
