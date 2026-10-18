-- The cursor only moves forward: a stale device cannot mark messages unread again.
-- name: MarkChannelRead :one
INSERT INTO channel_reads (channel_id, user_id, last_read_seq)
VALUES ($1, $2, $3)
ON CONFLICT (channel_id, user_id) DO UPDATE
    SET last_read_seq = greatest(channel_reads.last_read_seq, excluded.last_read_seq),
        updated_at = now()
RETURNING *;

-- Unread and mention counts for every channel of one workspace in a single query (task 2.8).
-- name: CountWorkspaceUnread :many
SELECT channels.id AS channel_id,
       coalesce(channel_reads.last_read_seq, 0)::bigint AS last_read_seq,
       count(messages.seq) AS unread,
       count(message_mentions.user_id) AS mentions
FROM channels
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
WHERE channels.workspace_id = sqlc.arg(workspace_id)::uuid
GROUP BY channels.id, channel_reads.last_read_seq;

-- The counts of one channel, for the answer after the cursor moved.
-- name: CountChannelUnread :one
SELECT coalesce(channel_reads.last_read_seq, 0)::bigint AS last_read_seq,
       count(messages.seq) AS unread,
       count(message_mentions.user_id) AS mentions
FROM channels
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
WHERE channels.id = sqlc.arg(channel_id)::uuid
GROUP BY channels.id, channel_reads.last_read_seq;
