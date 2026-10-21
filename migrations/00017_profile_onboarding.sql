-- +goose Up
-- The one-time "fill in your profile" step (SCRUM-108). NULL means the account
-- has not passed it yet, which is every account until it saves or skips once.
-- The column is about its owner only: it never becomes part of the public
-- profile and is read through the self profile alone.
ALTER TABLE users
    ADD COLUMN profile_onboarded_at timestamptz;

-- +goose Down
ALTER TABLE users
    DROP COLUMN profile_onboarded_at;
