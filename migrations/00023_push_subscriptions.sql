-- +goose Up
-- Web Push subscriptions (internal/push): one row per browser that agreed to
-- notifications. The endpoint is the push service's URL for that browser and is
-- unique: the same browser signing in as someone else moves the row to the new
-- account instead of notifying both. p256dh and auth are the browser's keys the
-- payload is encrypted with (RFC 8291); they are not secrets of the account.
CREATE TABLE push_subscriptions (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    endpoint        text NOT NULL UNIQUE CHECK (char_length(endpoint) BETWEEN 1 AND 2048),
    p256dh          text NOT NULL CHECK (char_length(p256dh) BETWEEN 1 AND 256),
    auth            text NOT NULL CHECK (char_length(auth) BETWEEN 1 AND 256),
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_success_at timestamptz
);

CREATE INDEX push_subscriptions_user_id_idx ON push_subscriptions (user_id);

-- +goose Down
DROP TABLE push_subscriptions;
