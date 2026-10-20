-- +goose Up
-- Who may add an account as a friend (SCRUM-98). "shared" is the safe default:
-- a request needs a common server, which is the same proof of acquaintance the
-- direct message policy uses. "all" opens it to any non-guest account, "nobody"
-- closes it; a block outranks all three.
ALTER TABLE users ADD COLUMN friend_request_policy text NOT NULL DEFAULT 'shared',
    ADD CONSTRAINT users_friend_request_policy_valid
        CHECK (friend_request_policy IN ('all', 'shared', 'nobody'));

-- One row per pair, whichever direction the request went. The pair is stored
-- with the smaller id first, so (user_low, user_high) is both the uniqueness
-- rule and the lookup a request, an accept or a block needs. The row keeps
-- its identity when a request becomes a friendship: status changes in place,
-- so a request that is accepted cannot race into a second row.
CREATE TABLE friendships (
    user_low     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    user_high    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    status       text        NOT NULL,
    requested_by uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    accepted_at  timestamptz,
    PRIMARY KEY (user_low, user_high),
    CONSTRAINT friendships_pair_ordered CHECK (user_low < user_high),
    CONSTRAINT friendships_status_valid CHECK (status IN ('pending', 'accepted')),
    CONSTRAINT friendships_requester_is_member CHECK (requested_by IN (user_low, user_high)),
    -- An accepted row always has the moment it was accepted; a pending one never.
    CONSTRAINT friendships_accepted_at_matches_status CHECK (
        (status = 'accepted' AND accepted_at IS NOT NULL)
        OR (status = 'pending' AND accepted_at IS NULL)
    )
);

-- Both directions of "my rows": a pair may put the account in either column.
-- These indexes serve a friend list, both request lists and a block check.
CREATE INDEX friendships_user_low_status_idx ON friendships (user_low, status, created_at DESC);
CREATE INDEX friendships_user_high_status_idx ON friendships (user_high, status, created_at DESC);
-- The outgoing list is exactly "requests this account sent", newest first.
CREATE INDEX friendships_requested_by_status_idx ON friendships (requested_by, status, created_at DESC);

-- +goose Down
DROP INDEX friendships_requested_by_status_idx;
DROP INDEX friendships_user_high_status_idx;
DROP INDEX friendships_user_low_status_idx;
DROP TABLE friendships;
ALTER TABLE users DROP CONSTRAINT users_friend_request_policy_valid;
ALTER TABLE users DROP COLUMN friend_request_policy;
