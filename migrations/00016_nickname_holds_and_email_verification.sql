-- +goose Up
-- A nickname that someone has just given up stays out of circulation for a
-- week (SCRUM-104): otherwise another account could take the old handle at
-- once and impersonate its previous owner. The table is the record of that
-- hold; the trigger below is what enforces it, so a registration and a rename
-- cannot race past a check-then-write.
CREATE TABLE released_nicknames (
    nickname_key text        PRIMARY KEY,
    released_at  timestamptz NOT NULL DEFAULT now(),
    released_by  uuid        REFERENCES users (id) ON DELETE SET NULL
);

-- No email confirmation flow exists yet. The column is where one will later
-- record success; every address a person writes by hand stays unverified.
ALTER TABLE users
    ADD COLUMN email_verified_at timestamptz;

-- The hold is checked inside the same statement that writes the row, not by
-- application code: a unique index already settles two people asking for one
-- free nickname, and this trigger settles a third asking for a held one.
-- +goose StatementBegin
CREATE FUNCTION users_nickname_not_released() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM released_nicknames
        WHERE nickname_key = lower(NEW.nickname)
          AND released_at > now() - interval '7 days'
    ) THEN
        RAISE EXCEPTION 'nickname is reserved for 7 days after release'
            USING ERRCODE = 'P0001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER users_nickname_not_released
    BEFORE INSERT OR UPDATE OF nickname ON users
    FOR EACH ROW
    EXECUTE FUNCTION users_nickname_not_released();

-- +goose Down
DROP TRIGGER users_nickname_not_released ON users;
DROP FUNCTION users_nickname_not_released();
ALTER TABLE users DROP COLUMN email_verified_at;
DROP TABLE released_nicknames;
