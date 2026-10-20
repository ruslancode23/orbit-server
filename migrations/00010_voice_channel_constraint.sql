-- +goose Up
-- Text and voice channels both belong to a server and have a name; direct and
-- group conversations have neither. The old check only knew about text.
ALTER TABLE channels DROP CONSTRAINT channels_text_in_workspace;
ALTER TABLE channels ADD CONSTRAINT channels_server_channel_in_workspace CHECK (
    (kind IN ('text', 'voice') AND workspace_id IS NOT NULL AND name IS NOT NULL)
    OR (kind IN ('direct', 'group') AND workspace_id IS NULL AND name IS NULL)
);

-- +goose Down
ALTER TABLE channels DROP CONSTRAINT channels_server_channel_in_workspace;
ALTER TABLE channels ADD CONSTRAINT channels_text_in_workspace CHECK (
    (kind = 'text' AND workspace_id IS NOT NULL AND name IS NOT NULL)
    OR (kind <> 'text' AND workspace_id IS NULL AND name IS NULL)
);
