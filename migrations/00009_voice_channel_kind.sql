-- +goose Up
-- A channel of a server may carry voice as well as text. The room itself lives
-- in LiveKit; the channel row is what gives it a name, a place and permissions.
ALTER TYPE channel_kind ADD VALUE IF NOT EXISTS 'voice';

-- +goose Down
-- PostgreSQL cannot drop a single enum value; an unused 'voice' is harmless.
SELECT 1;
