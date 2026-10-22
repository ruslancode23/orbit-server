-- +goose Up
-- Bug reports sent from inside the app (internal/feedback). The text is what
-- the person typed; the rest is the context the client attaches by itself. The
-- account is kept for follow-up questions, and the report outlives the account:
-- deleting a user leaves the report without an author rather than losing it.
-- The screenshot lives in the object store under screenshot_key.
CREATE TABLE bug_reports (
    id               uuid PRIMARY KEY,
    user_id          uuid REFERENCES users (id) ON DELETE SET NULL,
    description      text NOT NULL CHECK (char_length(btrim(description)) BETWEEN 1 AND 4000),
    steps            text NOT NULL DEFAULT '' CHECK (char_length(steps) <= 4000),
    app_version      text NOT NULL CHECK (char_length(app_version) BETWEEN 1 AND 64),
    user_agent       text NOT NULL DEFAULT '' CHECK (char_length(user_agent) <= 512),
    route            text NOT NULL CHECK (char_length(route) BETWEEN 1 AND 512),
    screenshot_key   text,
    screenshot_type  text CHECK (screenshot_type IN ('image/png', 'image/jpeg', 'image/webp')),
    screenshot_bytes bigint CHECK (screenshot_bytes > 0),
    created_at       timestamptz NOT NULL DEFAULT now(),
    CHECK ((screenshot_key IS NULL) = (screenshot_type IS NULL)
       AND (screenshot_key IS NULL) = (screenshot_bytes IS NULL))
);

-- The team reads the newest first.
CREATE INDEX bug_reports_created_at_idx ON bug_reports (created_at DESC);
CREATE INDEX bug_reports_user_id_idx ON bug_reports (user_id);

-- +goose Down
DROP TABLE bug_reports;
