-- +goose Up
-- One-time password reset tokens (SCRUM-151). The client receives the raw token
-- in a link; the database keeps only its SHA-256, exactly like sessions: a
-- leaked table does not hand out resets. A token dies when it is used, when it
-- expires, or together with its account.
CREATE TABLE password_reset_tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash bytea       NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX password_reset_tokens_token_hash_key ON password_reset_tokens (token_hash);
-- Issuing a token removes the previous ones of the account; this is the lookup.
CREATE INDEX password_reset_tokens_user_id_idx ON password_reset_tokens (user_id);

-- +goose Down
DROP TABLE password_reset_tokens;
