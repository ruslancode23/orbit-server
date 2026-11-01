-- Tokens are stored as the SHA-256 of the raw value, like sessions. Issuing a
-- new token drops the unused ones of the account, so only the last link works.
-- name: CreatePasswordResetToken :exec
WITH cleared AS (
    DELETE FROM password_reset_tokens
    WHERE user_id = sqlc.arg(account_id)
      AND used_at IS NULL
)
INSERT INTO password_reset_tokens (user_id, token_hash, expires_at)
VALUES (sqlc.arg(account_id), sqlc.arg(hashed_token), sqlc.arg(expires_at));

-- One statement consumes the token, writes the new password and revokes every
-- session: either all three happen or none. A used, expired or unknown token
-- returns no row, which the caller turns into one generic refusal.
-- name: ResetPasswordWithToken :one
WITH consumed AS (
    UPDATE password_reset_tokens
    SET used_at = now()
    WHERE password_reset_tokens.token_hash = sqlc.arg(hashed_token)
      AND used_at IS NULL
      AND expires_at > now()
    RETURNING user_id
), changed AS (
    UPDATE users
    SET password_hash = sqlc.arg(new_password_hash), updated_at = now()
    WHERE id IN (SELECT user_id FROM consumed)
    RETURNING id
), revoked AS (
    UPDATE sessions
    SET revoked_at = now()
    WHERE user_id IN (SELECT user_id FROM consumed)
      AND revoked_at IS NULL
)
SELECT user_id FROM consumed;
