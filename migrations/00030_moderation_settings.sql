-- +goose Up
-- Moderation settings (SCRUM-163): channel slowmode, timeouts, the automute of
-- new members and the per-message caps on mentions and links.
--
-- A channel decides how often one author may write in it: between two messages
-- of the same account at least slowmode_seconds must pass. Zero turns it off;
-- six hours is as slow as a channel may be, because a longer wait makes the
-- setting indistinguishable from a mute.
ALTER TABLE channels
    ADD COLUMN slowmode_seconds integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT channels_slowmode_range CHECK (slowmode_seconds BETWEEN 0 AND 21600);

-- A timeout: until this moment the member may read, receive events and sit in
-- voice, but may not write in the server's channels. The reason is kept with
-- the mute for the moderators' journal and is never published to a client as
-- part of an event.
ALTER TABLE workspace_members
    ADD COLUMN muted_until timestamptz,
    ADD COLUMN mute_reason text,
    ADD CONSTRAINT workspace_members_mute_reason_length CHECK (
        mute_reason IS NULL OR char_length(mute_reason) <= 500
    );

-- The slowmode clock is the author's last message in the channel: one indexed
-- lookup per send, not a scan of the history.
CREATE INDEX messages_channel_author_created_idx
    ON messages (channel_id, author_id, created_at DESC);

-- Server-wide settings. New members may be muted on arrival for up to a week;
-- one message may name at most max_mentions_per_message accounts and carry at
-- most max_links_per_message links. Zero turns each cap off, and the mention
-- and link caps default to the five the task names.
ALTER TABLE workspaces
    ADD COLUMN automute_new_members_seconds integer NOT NULL DEFAULT 0,
    ADD COLUMN max_mentions_per_message      integer NOT NULL DEFAULT 5,
    ADD COLUMN max_links_per_message         integer NOT NULL DEFAULT 5,
    ADD CONSTRAINT workspaces_automute_range CHECK (
        automute_new_members_seconds BETWEEN 0 AND 604800
    ),
    ADD CONSTRAINT workspaces_max_mentions_range CHECK (max_mentions_per_message BETWEEN 0 AND 100),
    ADD CONSTRAINT workspaces_max_links_range CHECK (max_links_per_message BETWEEN 0 AND 100);

-- +goose Down
ALTER TABLE workspaces
    DROP COLUMN max_links_per_message,
    DROP COLUMN max_mentions_per_message,
    DROP COLUMN automute_new_members_seconds;

DROP INDEX messages_channel_author_created_idx;

ALTER TABLE workspace_members
    DROP COLUMN mute_reason,
    DROP COLUMN muted_until;

ALTER TABLE channels DROP COLUMN slowmode_seconds;
