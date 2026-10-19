-- +goose Up
-- What a server shows about itself: a short description and an icon. The icon
-- bytes live in the object store; the row keeps only the key, and members get a
-- signed link to it, never the key itself.
ALTER TABLE workspaces
    ADD COLUMN description text,
    ADD COLUMN icon_key    text,
    ADD CONSTRAINT workspaces_description_length CHECK (description IS NULL OR char_length(description) BETWEEN 1 AND 500);

-- +goose Down
ALTER TABLE workspaces
    DROP CONSTRAINT workspaces_description_length,
    DROP COLUMN icon_key,
    DROP COLUMN description;
