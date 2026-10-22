-- +goose Up
-- Inserting a role above @everyone and reordering roles shift many positions
-- at once; a unique index checks each row as it changes, so a shift by one
-- collides with itself. A deferred constraint checks the end of the
-- transaction instead: the order must be whole when it is committed.
DROP INDEX server_roles_position;
ALTER TABLE server_roles
    ADD CONSTRAINT server_roles_position UNIQUE (workspace_id, position) DEFERRABLE INITIALLY DEFERRED;

-- +goose Down
ALTER TABLE server_roles DROP CONSTRAINT server_roles_position;
CREATE UNIQUE INDEX server_roles_position ON server_roles (workspace_id, position);
