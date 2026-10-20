-- +goose Up
-- Who may start a direct conversation with an account (SCRUM-85). "shared"
-- means people on a common server, "nobody" means no one, and "friends" is
-- reserved for when friendships exist: it behaves as "nobody" until then.
ALTER TABLE users ADD COLUMN dm_policy text NOT NULL DEFAULT 'shared',
    ADD CONSTRAINT users_dm_policy_valid CHECK (dm_policy IN ('shared', 'friends', 'nobody'));

-- A block is one-sided in the table and two-sided in effect: neither side may
-- start a dialogue or write in the one they already have. The primary key makes
-- blocking twice a no-op; the reverse index answers "who blocked me" and the
-- pair lookup that every dialogue start runs.
CREATE TABLE user_blocks (
    blocker_id uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    blocked_id uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (blocker_id, blocked_id),
    CONSTRAINT user_blocks_not_self CHECK (blocker_id <> blocked_id)
);

CREATE INDEX user_blocks_blocked_id_idx ON user_blocks (blocked_id);

-- +goose Down
DROP INDEX user_blocks_blocked_id_idx;
DROP TABLE user_blocks;
ALTER TABLE users DROP CONSTRAINT users_dm_policy_valid;
ALTER TABLE users DROP COLUMN dm_policy;
