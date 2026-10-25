-- +goose Up
-- OpenGraph previews of the first link in a message (FEAT-8). The URL is the
-- normalized address; the row is a cache with a deadline, including failures
-- (status = 'error') so a broken link is not fetched on every message.
CREATE TABLE link_previews (
    id          uuid PRIMARY KEY,
    url         text NOT NULL UNIQUE CHECK (char_length(url) BETWEEN 1 AND 2048),
    title       text,
    description text,
    site_name   text,
    image_url   text,
    status      text NOT NULL CHECK (status IN ('ok', 'error')),
    fetched_at  timestamptz NOT NULL DEFAULT now()
);

-- One preview per message: the first link is the one shown. Removing it deletes
-- the row; editing the message attaches the preview of the new text.
CREATE TABLE message_previews (
    message_id uuid PRIMARY KEY REFERENCES messages (id) ON DELETE CASCADE,
    preview_id uuid NOT NULL REFERENCES link_previews (id) ON DELETE CASCADE
);

CREATE INDEX message_previews_preview_id_idx ON message_previews (preview_id);

-- +goose Down
DROP TABLE message_previews;
DROP TABLE link_previews;
