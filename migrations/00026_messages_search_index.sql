-- +goose NO TRANSACTION
-- +goose Up
-- The GIN index over the message vectors. Built concurrently so a busy server
-- keeps accepting writes while it is made; goose runs this file outside a
-- transaction for that reason.
CREATE INDEX CONCURRENTLY IF NOT EXISTS messages_search_idx
    ON messages USING GIN (search);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS messages_search_idx;
