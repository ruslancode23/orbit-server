-- +goose Up
-- Conversation list summaries and group management (DM-2, SCRUM-84).
--
-- A group may now have a name: a dialogue still has none, and a secret chat is
-- named by neither side. The old check only allowed a name on a server channel.
ALTER TABLE channels DROP CONSTRAINT channels_server_channel_in_workspace;
ALTER TABLE channels ADD CONSTRAINT channels_kind_shape CHECK (
    (kind IN ('text', 'voice') AND workspace_id IS NOT NULL AND name IS NOT NULL
        AND char_length(name) BETWEEN 1 AND 64)
    OR (kind = 'group' AND workspace_id IS NULL
        AND (name IS NULL OR char_length(name) BETWEEN 1 AND 100))
    OR (kind = 'direct' AND workspace_id IS NULL AND name IS NULL)
    OR (kind = 'secret' AND workspace_id IS NULL AND name IS NULL)
);

-- A conversation is hidden from one member's list, not from the others. The
-- number is the newest seq at the moment of hiding: a later message brings the
-- conversation back without a write on every send, and a race with a message
-- arriving at the same time is settled by seq rather than by a timestamp.
ALTER TABLE channel_members ADD COLUMN hidden_after_seq bigint;

-- System lines ("X added Y", "X left", "X renamed the chat") are ordinary
-- messages with their own kind: they keep a seq, appear in history and have an
-- author, but no mentions and no text the author typed. The structured type lets
-- a client render them in its own language; content carries a readable fallback.
CREATE TYPE message_kind AS ENUM ('user', 'system');

ALTER TABLE messages
    ADD COLUMN kind message_kind NOT NULL DEFAULT 'user',
    ADD COLUMN system_type text,
    ADD COLUMN system_target_id uuid REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN system_name text,
    ADD CONSTRAINT messages_system_shape CHECK (
        (kind = 'user'
            AND system_type IS NULL
            AND system_target_id IS NULL
            AND system_name IS NULL)
        OR (kind = 'system'
            AND system_type IN ('member_added', 'member_removed', 'conversation_renamed')
            AND (system_name IS NULL OR char_length(system_name) BETWEEN 1 AND 100))
    );

-- The list reads the newest message of each conversation and counts the unread
-- ones after the cursor; both walk channel_id in seq order.
CREATE INDEX messages_channel_kind_idx ON messages (channel_id, seq DESC)
    INCLUDE (author_id, created_at, kind);

-- Counting mentions of one account among the unread messages joins on this pair.
CREATE INDEX message_mentions_user_message_idx ON message_mentions (user_id, message_id);

-- The list skips hidden conversations unless a newer message has arrived.
CREATE INDEX channel_members_user_visible_idx ON channel_members (user_id, channel_id)
    WHERE hidden_after_seq IS NULL;

-- +goose Down
DROP INDEX channel_members_user_visible_idx;
DROP INDEX message_mentions_user_message_idx;
DROP INDEX messages_channel_kind_idx;

ALTER TABLE messages
    DROP CONSTRAINT messages_system_shape,
    DROP COLUMN system_name,
    DROP COLUMN system_target_id,
    DROP COLUMN system_type,
    DROP COLUMN kind;

DROP TYPE message_kind;

ALTER TABLE channel_members DROP COLUMN hidden_after_seq;

ALTER TABLE channels DROP CONSTRAINT channels_kind_shape;
ALTER TABLE channels ADD CONSTRAINT channels_server_channel_in_workspace CHECK (
    (kind IN ('text', 'voice') AND workspace_id IS NOT NULL AND name IS NOT NULL)
    OR (kind IN ('direct', 'group') AND workspace_id IS NULL AND name IS NULL)
);
