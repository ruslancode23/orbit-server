-- +goose Up
-- What the member role may do on one server. The owner and admins may always do
-- all of it. The defaults keep what members could already do: attach files, but
-- neither invite people nor create channels.
ALTER TABLE workspaces
    ADD COLUMN members_can_invite          boolean NOT NULL DEFAULT false,
    ADD COLUMN members_can_create_channels boolean NOT NULL DEFAULT false,
    ADD COLUMN members_can_attach_files    boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE workspaces
    DROP COLUMN members_can_attach_files,
    DROP COLUMN members_can_create_channels,
    DROP COLUMN members_can_invite;
