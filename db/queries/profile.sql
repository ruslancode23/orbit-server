-- What a person shows about themselves (SCRUM-103). These queries own the
-- profile columns of users; internal/auth keeps owning the rest of the row.
-- The avatar key is read here but never leaves the profile module: the API
-- carries a signed link instead.

-- name: GetUserProfile :one
SELECT id, nickname, display_name, bio, accent_color, avatar_key,
       custom_status_emoji, custom_status_text, custom_status_expires_at,
       profile_onboarded_at, is_guest, created_at
FROM users
WHERE id = $1;

-- One query for a whole member list or message page: a reader must never ask
-- per author, or a page of history costs one query per message.
-- name: ListUserProfiles :many
SELECT id, nickname, display_name, bio, accent_color, avatar_key,
       custom_status_emoji, custom_status_text, custom_status_expires_at,
       profile_onboarded_at, is_guest, created_at
FROM users
WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- Only the fields the caller sent change. An empty display_name or bio clears
-- it; the display name then falls back to the nickname. An accent color is a
-- complete replacement, never a partial one.
-- name: UpdateUserProfile :one
UPDATE users
SET display_name     = CASE WHEN sqlc.arg(set_display_name)::bool THEN sqlc.narg(display_name) ELSE display_name END,
    bio              = CASE WHEN sqlc.arg(set_bio)::bool THEN sqlc.narg(bio) ELSE bio END,
    accent_color     = coalesce(sqlc.narg(accent_color), accent_color),
    profile_updated_at = now(),
    updated_at       = now()
WHERE id = sqlc.arg(id)
RETURNING id, nickname, display_name, bio, accent_color, avatar_key,
          custom_status_emoji, custom_status_text, custom_status_expires_at,
          profile_onboarded_at, is_guest, created_at;

-- A status replaces the old one entirely: clearing one of its two parts must
-- not leave the other behind. An expiry is part of the status, not a separate
-- setting.
-- name: SetUserStatus :one
UPDATE users
SET custom_status_emoji      = sqlc.narg(emoji),
    custom_status_text       = sqlc.narg(status_text),
    custom_status_expires_at = sqlc.narg(expires_at),
    profile_updated_at       = now(),
    updated_at               = now()
WHERE id = sqlc.arg(id)
RETURNING id, nickname, display_name, bio, accent_color, avatar_key,
          custom_status_emoji, custom_status_text, custom_status_expires_at,
          profile_onboarded_at, is_guest, created_at;

-- name: ClearUserStatus :one
UPDATE users
SET custom_status_emoji      = NULL,
    custom_status_text       = NULL,
    custom_status_expires_at = NULL,
    profile_updated_at       = now(),
    updated_at               = now()
WHERE id = sqlc.arg(id)
RETURNING id, nickname, display_name, bio, accent_color, avatar_key,
          custom_status_emoji, custom_status_text, custom_status_expires_at,
          profile_onboarded_at, is_guest, created_at;

-- One statement reads the old key and writes the new one, so two uploads racing
-- cannot both report the same previous key and leave one object alive.
-- name: SetUserAvatar :one
WITH previous AS (
    SELECT avatar_key FROM users WHERE users.id = sqlc.arg(id)
)
UPDATE users
SET avatar_key = sqlc.narg(avatar_key)::text,
    profile_updated_at = now(),
    updated_at = now()
WHERE users.id = sqlc.arg(id)
RETURNING id, nickname, display_name, bio, accent_color, avatar_key,
          custom_status_emoji, custom_status_text, custom_status_expires_at,
          profile_onboarded_at, is_guest, created_at,
          (SELECT avatar_key FROM previous) AS previous_avatar_key;

-- The first timestamp stays: a retry after a dropped response changes nothing,
-- so "save" and "skip" can both call this without a check. This is not profile
-- content, so neither updated_at nor profile_updated_at moves, and nobody else
-- hears about it.
-- name: MarkUserProfileOnboarded :execrows
UPDATE users
SET profile_onboarded_at = coalesce(profile_onboarded_at, sqlc.arg(at))
WHERE id = sqlc.arg(id);
