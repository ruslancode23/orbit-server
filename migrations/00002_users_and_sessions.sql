-- +goose Up
-- Accounts and opaque sessions (FR-01). A guest has no password and no email until it signs up.
CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    nickname      text        NOT NULL,
    email         text,
    password_hash text,
    is_guest      boolean     NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_nickname_length CHECK (char_length(nickname) BETWEEN 1 AND 32),
    -- Only a guest may be without a password; everyone else signs in with one.
    CONSTRAINT users_password_or_guest CHECK (is_guest OR password_hash IS NOT NULL)
);

-- Case-insensitive uniqueness: "Anna" and "anna" are the same account.
CREATE UNIQUE INDEX users_nickname_key ON users (lower(nickname));
CREATE UNIQUE INDEX users_email_key ON users (lower(email)) WHERE email IS NOT NULL;

-- Only the hash is stored: a leaked table does not hand out live sessions.
CREATE TABLE sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   bytea       NOT NULL,
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX sessions_token_hash_key ON sessions (token_hash);
-- Listing and revoking every session of one user (password change, ban).
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- +goose Down
DROP TABLE sessions;
DROP TABLE users;
