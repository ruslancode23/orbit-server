-- +goose Up
-- Notification levels per account (internal/notifysettings): how loudly a
-- server, a channel or a conversation speaks on every device of one account.
-- `level` NULL means "inherit": a channel takes the server's level, a
-- conversation has nothing to inherit and falls back to "all". `muted_until`
-- silences whatever the level says until that moment; the latest deadline on
-- the way (channel and server) wins. The target is not a foreign key because it
-- names one of three tables: workspaces, channels (server channels and
-- conversations).
CREATE TABLE notification_settings (
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    scope       text NOT NULL CHECK (scope IN ('workspace', 'channel', 'conversation')),
    target_id   uuid NOT NULL,
    level       text CHECK (level IN ('all', 'mentions', 'none')),
    muted_until timestamptz,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, scope, target_id)
);

-- A message asks every level of one channel and of its server at once.
CREATE INDEX notification_settings_target_idx ON notification_settings (scope, target_id);

-- +goose Down
DROP TABLE notification_settings;
