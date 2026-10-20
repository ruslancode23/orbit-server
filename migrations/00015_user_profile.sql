-- +goose Up
-- What people show about themselves (SCRUM-103). The unique @nickname stays the
-- handle: display_name is only what the UI draws, and mentions keep using the
-- nickname. A NULL display name means "same as the nickname", so an account that
-- never set one still has a name. The profile columns belong to internal/profile;
-- internal/auth keeps owning the rest of the row.
ALTER TABLE users
    ADD COLUMN display_name             text,
    ADD COLUMN bio                      text NOT NULL DEFAULT '',
    ADD COLUMN accent_color             text NOT NULL DEFAULT 'slate',
    ADD COLUMN avatar_key               text,
    ADD COLUMN custom_status_emoji      text,
    ADD COLUMN custom_status_text       text,
    ADD COLUMN custom_status_expires_at timestamptz,
    ADD COLUMN profile_updated_at       timestamptz NOT NULL DEFAULT now(),
    ADD CONSTRAINT users_display_name_length CHECK (display_name IS NULL OR char_length(display_name) BETWEEN 1 AND 32),
    ADD CONSTRAINT users_bio_length CHECK (char_length(bio) <= 190),
    -- A named palette token, not a raw hex colour: the client maps the token to
    -- whatever its theme uses, so a profile renders correctly in every theme.
    ADD CONSTRAINT users_accent_color_valid CHECK (accent_color IN (
        'slate', 'red', 'amber', 'green', 'teal', 'sky', 'indigo', 'violet', 'pink'
    )),
    -- The key itself never leaves the server: responses carry a signed link.
    ADD CONSTRAINT users_avatar_key_length CHECK (avatar_key IS NULL OR char_length(avatar_key) BETWEEN 1 AND 300),
    ADD CONSTRAINT users_custom_status_emoji_length CHECK (custom_status_emoji IS NULL OR char_length(custom_status_emoji) BETWEEN 1 AND 32),
    ADD CONSTRAINT users_custom_status_text_length CHECK (custom_status_text IS NULL OR char_length(custom_status_text) BETWEEN 1 AND 128),
    -- A status is either absent or has something to show; an expiry alone is not
    -- a status. An expired status stays in the row and is hidden by the server,
    -- so no background job is needed just to clear it.
    ADD CONSTRAINT users_custom_status_complete CHECK (
        (custom_status_emoji IS NULL AND custom_status_text IS NULL AND custom_status_expires_at IS NULL)
        OR custom_status_emoji IS NOT NULL
        OR custom_status_text IS NOT NULL
    );

-- +goose Down
ALTER TABLE users
    DROP CONSTRAINT users_custom_status_complete,
    DROP CONSTRAINT users_custom_status_text_length,
    DROP CONSTRAINT users_custom_status_emoji_length,
    DROP CONSTRAINT users_avatar_key_length,
    DROP CONSTRAINT users_accent_color_valid,
    DROP CONSTRAINT users_bio_length,
    DROP CONSTRAINT users_display_name_length,
    DROP COLUMN profile_updated_at,
    DROP COLUMN custom_status_expires_at,
    DROP COLUMN custom_status_text,
    DROP COLUMN custom_status_emoji,
    DROP COLUMN avatar_key,
    DROP COLUMN accent_color,
    DROP COLUMN bio,
    DROP COLUMN display_name;
