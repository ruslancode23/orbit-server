-- +goose Up
-- A group title may be up to 100 characters (00018), but the column-wide check
-- from 00003 still capped every channel name at 64, so a longer rename failed in
-- the database instead of being saved. The lengths now live in
-- channels_kind_shape alone: 64 for a server channel, 100 for a group.
ALTER TABLE channels DROP CONSTRAINT channels_name_length;

-- +goose Down
UPDATE channels SET name = left(name, 64) WHERE char_length(name) > 64;
ALTER TABLE channels
    ADD CONSTRAINT channels_name_length CHECK (name IS NULL OR char_length(name) BETWEEN 1 AND 64);
