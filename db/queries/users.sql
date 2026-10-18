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
