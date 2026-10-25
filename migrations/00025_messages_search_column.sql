-- +goose Up
-- Full-text search over message text (FEAT-6): Russian and English dictionaries
-- in one stored vector. Generated columns keep it in step with edits, and the
-- expression is immutable because the configuration is spelled out.
--
-- Adding a stored generated column rewrites the table once. On a large
-- production table this migration takes time and an exclusive lock; the index
-- is built separately and concurrently in 00026.
ALTER TABLE messages
    ADD COLUMN search tsvector GENERATED ALWAYS AS (
        to_tsvector('russian', content) || to_tsvector('english', content)
    ) STORED;

-- +goose Down
ALTER TABLE messages DROP COLUMN search;
