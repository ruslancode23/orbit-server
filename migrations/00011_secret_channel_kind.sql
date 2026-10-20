-- +goose Up
-- A secret chat is a conversation whose contents never reach the server: only
-- ciphertext envelopes travel through it. It is still a channel, because that
-- is what gives it an id, participants and access rules. The value is added in
-- its own migration: PostgreSQL cannot use a new enum value in the transaction
-- that added it.
ALTER TYPE channel_kind ADD VALUE IF NOT EXISTS 'secret';

-- +goose Down
-- PostgreSQL cannot drop a single enum value; an unused 'secret' is harmless.
SELECT 1;
