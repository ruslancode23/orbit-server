-- +goose Up
-- Who is in a direct or group conversation. A channel of a server needs no such
-- table: membership in the server decides. A conversation has no server, so its
-- participants are listed here.
CREATE TABLE channel_members (
    channel_id uuid        NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    joined_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (channel_id, user_id)
);

-- "Which conversations am I in" on every page load.
CREATE INDEX channel_members_user_id_idx ON channel_members (user_id);

-- A pair of people has one direct conversation, not a new one per attempt.
-- The key is the sorted pair, so (anna, bob) and (bob, anna) are the same.
ALTER TABLE channels ADD COLUMN direct_key text;

CREATE UNIQUE INDEX channels_direct_key_key ON channels (direct_key) WHERE direct_key IS NOT NULL;

-- +goose Down
DROP INDEX channels_direct_key_key;
ALTER TABLE channels DROP COLUMN direct_key;
DROP TABLE channel_members;
