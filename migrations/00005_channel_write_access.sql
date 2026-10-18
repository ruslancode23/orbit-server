-- +goose Up
-- Who may write in a channel. Reading follows membership in the server; writing is
-- open to every member unless the channel is announcements-only (admins and the owner).
ALTER TABLE channels ADD COLUMN is_read_only boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE channels DROP COLUMN is_read_only;
