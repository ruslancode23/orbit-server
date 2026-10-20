-- name: CreateUser :one
INSERT INTO users (nickname, email, password_hash, is_guest)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByNickname :one
SELECT * FROM users WHERE lower(nickname) = lower($1);

-- name: GetUserByEmail :one
SELECT * FROM users WHERE lower(email) = lower($1);

-- name: SetUserPassword :exec
UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1;

-- A guest that signs up keeps its id, so its messages and memberships stay with it.
-- name: PromoteGuest :one
UPDATE users
SET nickname = $2, email = $3, password_hash = $4, is_guest = false, updated_at = now()
WHERE id = $1 AND is_guest
RETURNING *;

-- Sign-in accepts either a nickname or an email, in any case.
-- name: GetUserByLogin :one
SELECT * FROM users
WHERE lower(nickname) = lower($1) OR lower(email) = lower($1);

-- Which of these ids are real accounts (task 2.10 checks before it opens a chat).
-- name: ListKnownUsers :many
SELECT id FROM users WHERE id = ANY($1::uuid[]);

-- A released nickname stays unclaimable for seven days. The check is here for a
-- clear 409; the trigger on users is what makes it race-free.
-- name: NicknameIsHeld :one
SELECT EXISTS (
    SELECT 1 FROM released_nicknames
    WHERE nickname_key = lower(sqlc.arg(nickname)::text)
      AND released_at > sqlc.arg(since)
) AS held;

-- Renaming releases the old handle in the same statement that takes the new
-- one: no window in which neither row is visible. A case-only change keeps the
-- key and adds no hold. The unique index settles a name two accounts ask for,
-- and the trigger settles one still held.
-- name: ChangeUserNickname :one
WITH current AS (
    SELECT nickname FROM users WHERE id = sqlc.arg(id)
), released AS (
    INSERT INTO released_nicknames (nickname_key, released_at, released_by)
    SELECT lower(current.nickname), now(), sqlc.arg(id)
    FROM current
    WHERE lower(current.nickname) <> lower(sqlc.arg(nickname)::text)
    ON CONFLICT (nickname_key) DO UPDATE
        SET released_at = EXCLUDED.released_at, released_by = EXCLUDED.released_by
    RETURNING nickname_key
)
UPDATE users
SET nickname = sqlc.arg(nickname)::text,
    profile_updated_at = now(),
    updated_at = now()
WHERE users.id = sqlc.arg(id)
RETURNING *;

-- There is no confirmation email yet, so a hand-written address is unverified.
-- An empty address clears it; the unique index allows any number of NULLs.
-- name: SetUserEmail :one
UPDATE users
SET email = sqlc.narg(email)::text,
    email_verified_at = NULL,
    updated_at = now()
WHERE users.id = sqlc.arg(id)
RETURNING *;
