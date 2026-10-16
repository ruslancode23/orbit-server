-- +goose Up
-- Messages, their mentions, reactions and attachments (FR-04, FR-05), plus read cursors.
CREATE TABLE messages (
    -- seq orders a channel by itself: two messages written in the same millisecond still
    -- have a stable order, and a read cursor is one comparable number.
    seq               bigserial   NOT NULL UNIQUE,
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    channel_id        uuid        NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    -- History outlives the account: a deleted user leaves its messages behind.
    author_id         uuid        REFERENCES users (id) ON DELETE SET NULL,
    -- The client's own id makes a resend after a network failure idempotent (FR-04).
    client_message_id text        NOT NULL,
    content           text        NOT NULL,
    reply_to_id       uuid        REFERENCES messages (id) ON DELETE SET NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    edited_at         timestamptz,
    -- Soft delete: replies and read cursors keep pointing at a deleted message.
    deleted_at        timestamptz,
    CONSTRAINT messages_content_length CHECK (char_length(content) <= 4000)
);

-- The history of one channel, newest first, by cursor.
CREATE INDEX messages_channel_id_seq_idx ON messages (channel_id, seq DESC);
-- A resend finds its own message; NULL author_id (deleted user) cannot collide.
CREATE UNIQUE INDEX messages_client_message_id_key ON messages (channel_id, author_id, client_message_id)
    WHERE author_id IS NOT NULL;
CREATE INDEX messages_reply_to_id_idx ON messages (reply_to_id) WHERE reply_to_id IS NOT NULL;

-- Mentions are stored, not parsed out of the text on every read: the unread
-- counter needs "mentions of me in this channel" as a query (task 2.8).
CREATE TABLE message_mentions (
    message_id uuid NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    PRIMARY KEY (message_id, user_id)
);

CREATE INDEX message_mentions_user_id_idx ON message_mentions (user_id);

CREATE TABLE message_reactions (
    message_id uuid        NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    emoji      text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, user_id, emoji),
    CONSTRAINT message_reactions_emoji_length CHECK (char_length(emoji) BETWEEN 1 AND 32)
);

CREATE TABLE attachments (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- NULL until the message is sent: the file is uploaded first.
    message_id  uuid        REFERENCES messages (id) ON DELETE CASCADE,
    uploader_id uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    storage_key text        NOT NULL,
    file_name   text        NOT NULL,
    mime_type   text        NOT NULL,
    size_bytes  bigint      NOT NULL,
    width       integer,
    height      integer,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT attachments_size_positive CHECK (size_bytes > 0)
);

CREATE UNIQUE INDEX attachments_storage_key_key ON attachments (storage_key);
CREATE INDEX attachments_message_id_idx ON attachments (message_id) WHERE message_id IS NOT NULL;
-- The sweep for files whose message never arrived.
CREATE INDEX attachments_orphans_idx ON attachments (created_at) WHERE message_id IS NULL;

-- How far one user has read one channel; synchronised between their devices (FR-04).
CREATE TABLE channel_reads (
    channel_id    uuid        NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    last_read_seq bigint      NOT NULL DEFAULT 0,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (channel_id, user_id)
);

-- Unread counts for every channel of a user in one query.
CREATE INDEX channel_reads_user_id_idx ON channel_reads (user_id);

-- +goose Down
DROP TABLE channel_reads;
DROP TABLE attachments;
DROP TABLE message_reactions;
DROP TABLE message_mentions;
DROP TABLE messages;
