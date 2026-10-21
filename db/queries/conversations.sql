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

-- One page of the conversation list with everything a sidebar row needs: the
-- newest message, the unread and mention counts after this account's cursor,
-- and the group name. It is one query for the whole page; the store then loads
-- the members of these conversations in one more, never one per conversation.
-- The cursor is (last_seq, created_at, id): a message seq orders the list, the
-- other two break ties between conversations that never had a message, so a
-- page boundary is stable even when activity is equal.
-- name: ListConversationSummaries :many
WITH visible AS (
    SELECT
        channels.id,
        channels.kind,
        channels.name,
        channels.created_at,
        coalesce(last_message.seq, 0)::bigint AS last_seq,
        last_message.id AS last_message_id,
        last_message.seq AS last_message_seq,
        last_message.author_id AS last_message_author_id,
        left(coalesce(last_message.content, ''), 120)::text AS last_message_content,
        coalesce(last_message.kind, 'user')::message_kind AS last_message_kind,
        coalesce(last_message.system_type, '')::text AS last_message_system_type,
        last_message.system_target_id AS last_message_system_target_id,
        coalesce(last_message.system_name, '')::text AS last_message_system_name,
        last_message.created_at AS last_message_created_at,
        last_message.deleted_at AS last_message_deleted_at,
        coalesce(channel_reads.last_read_seq, 0)::bigint AS last_read_seq,
        coalesce(counts.unread, 0)::bigint AS unread,
        coalesce(counts.mentions, 0)::bigint AS mentions
    FROM channel_members
        JOIN channels ON channels.id = channel_members.channel_id
        LEFT JOIN channel_reads
            ON channel_reads.channel_id = channels.id
           AND channel_reads.user_id = sqlc.arg(user_id)::uuid
        LEFT JOIN LATERAL (
            SELECT messages.id, messages.seq, messages.author_id, messages.content,
                   messages.kind, messages.system_type, messages.system_target_id,
                   messages.system_name, messages.created_at, messages.deleted_at
            FROM messages
            WHERE messages.channel_id = channels.id
            ORDER BY messages.seq DESC
            LIMIT 1
        ) AS last_message ON true
        LEFT JOIN LATERAL (
            SELECT
                count(*)::bigint AS unread,
                count(message_mentions.user_id)::bigint AS mentions
            FROM messages
                LEFT JOIN message_mentions
                    ON message_mentions.message_id = messages.id
                   AND message_mentions.user_id = sqlc.arg(user_id)::uuid
            WHERE messages.channel_id = channels.id
              AND messages.deleted_at IS NULL
              AND messages.author_id IS DISTINCT FROM sqlc.arg(user_id)::uuid
              AND messages.seq > coalesce(channel_reads.last_read_seq, 0)
        ) AS counts ON true
    WHERE channel_members.user_id = sqlc.arg(user_id)::uuid
      AND channels.workspace_id IS NULL
      -- A secret chat is not a dialogue in the ordinary sense: it lives on one
      -- device and has its own endpoints, so the conversation list leaves it out.
      AND channels.kind IN ('direct', 'group')
      -- Hiding is "from this message on": one that arrives later is newer than
      -- hidden_after_seq and brings the conversation back by itself.
      AND (
          channel_members.hidden_after_seq IS NULL
          OR coalesce(last_message.seq, 0) > channel_members.hidden_after_seq
      )
)
SELECT *
FROM visible
WHERE sqlc.narg(cursor_seq)::bigint IS NULL
   OR last_seq < sqlc.narg(cursor_seq)::bigint
   OR (last_seq = sqlc.narg(cursor_seq)::bigint AND created_at < sqlc.narg(cursor_created_at)::timestamptz)
   OR (last_seq = sqlc.narg(cursor_seq)::bigint
       AND created_at = sqlc.narg(cursor_created_at)::timestamptz
       AND id < sqlc.narg(cursor_id)::uuid)
ORDER BY last_seq DESC, created_at DESC, id DESC
LIMIT sqlc.arg(page_size)::integer;

-- The participants of a whole page of conversations in one query: internal/profile
-- then fills their public part in one batch, so a list never reads per row.
-- name: ListConversationMembersForChannels :many
SELECT channel_members.channel_id, channel_members.user_id, channel_members.joined_at
FROM channel_members
WHERE channel_members.channel_id = ANY(sqlc.arg(channel_ids)::uuid[])
ORDER BY channel_members.channel_id, channel_members.joined_at;

-- The badge of "direct messages": unread and mentions over all ordinary
-- conversations, with hidden ones left out exactly as the list leaves them out.
-- name: CountConversationUnread :one
SELECT
    coalesce(count(messages.seq), 0)::bigint AS unread,
    coalesce(count(message_mentions.user_id), 0)::bigint AS mentions
FROM channel_members
    JOIN channels ON channels.id = channel_members.channel_id
    LEFT JOIN channel_reads
        ON channel_reads.channel_id = channels.id
       AND channel_reads.user_id = sqlc.arg(user_id)::uuid
    LEFT JOIN messages
        ON messages.channel_id = channels.id
       AND messages.deleted_at IS NULL
       AND messages.author_id IS DISTINCT FROM sqlc.arg(user_id)::uuid
       AND messages.seq > coalesce(channel_reads.last_read_seq, 0)
    LEFT JOIN message_mentions
        ON message_mentions.message_id = messages.id
       AND message_mentions.user_id = sqlc.arg(user_id)::uuid
WHERE channel_members.user_id = sqlc.arg(user_id)::uuid
  AND channels.workspace_id IS NULL
  AND channels.kind IN ('direct', 'group')
  AND (
      channel_members.hidden_after_seq IS NULL
      OR EXISTS (
          SELECT 1
          FROM messages AS newer
          WHERE newer.channel_id = channels.id
            AND newer.seq > channel_members.hidden_after_seq
      )
  );

-- Who is in the conversation among the candidate ids: adding only counts the
-- people who are actually new against maxMembers.
-- name: ListExistingConversationMembers :many
SELECT user_id
FROM channel_members
WHERE channel_id = sqlc.arg(channel_id)::uuid
  AND user_id = ANY(sqlc.arg(user_ids)::uuid[]);

-- The row lock serialises two additions to one group, so both cannot pass the
-- same maxMembers count.
-- name: LockConversation :one
SELECT id FROM channels WHERE id = sqlc.arg(id)::uuid FOR UPDATE;

-- name: AddConversationMembers :many
INSERT INTO channel_members (channel_id, user_id)
SELECT sqlc.arg(channel_id)::uuid, unnest(sqlc.arg(user_ids)::uuid[])
ON CONFLICT DO NOTHING
RETURNING user_id;

-- name: RemoveConversationMember :execrows
DELETE FROM channel_members
WHERE channel_id = sqlc.arg(channel_id)::uuid
  AND user_id = sqlc.arg(user_id)::uuid;

-- name: RenameConversation :exec
UPDATE channels
SET name = sqlc.arg(name), updated_at = now()
WHERE id = sqlc.arg(id)::uuid
  AND workspace_id IS NULL
  AND kind = 'group';

-- Hiding stores the newest seq at that moment; a later message is greater and
-- shows the conversation again without a write on every send.
-- name: HideConversation :execrows
UPDATE channel_members
SET hidden_after_seq = coalesce((
    SELECT max(messages.seq) FROM messages WHERE messages.channel_id = channel_members.channel_id
), 0)
WHERE channel_id = sqlc.arg(channel_id)::uuid
  AND user_id = sqlc.arg(user_id)::uuid;

-- name: UnhideConversation :execrows
UPDATE channel_members
SET hidden_after_seq = NULL
WHERE channel_id = sqlc.arg(channel_id)::uuid
  AND user_id = sqlc.arg(user_id)::uuid;

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
