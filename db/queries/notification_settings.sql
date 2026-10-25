-- Notification levels (internal/notifysettings).

-- name: ListNotificationSettings :many
SELECT * FROM notification_settings WHERE user_id = sqlc.arg(user_id) ORDER BY scope, target_id;

-- name: GetNotificationSetting :one
SELECT * FROM notification_settings
WHERE user_id = sqlc.arg(user_id) AND scope = sqlc.arg(scope) AND target_id = sqlc.arg(target_id);

-- name: UpsertNotificationSetting :one
INSERT INTO notification_settings (user_id, scope, target_id, level, muted_until)
VALUES (sqlc.arg(user_id), sqlc.arg(scope), sqlc.arg(target_id), sqlc.arg(level), sqlc.arg(muted_until))
ON CONFLICT (user_id, scope, target_id) DO UPDATE
SET level = EXCLUDED.level, muted_until = EXCLUDED.muted_until, updated_at = now()
RETURNING *;

-- name: DeleteNotificationSetting :execrows
DELETE FROM notification_settings
WHERE user_id = sqlc.arg(user_id) AND scope = sqlc.arg(scope) AND target_id = sqlc.arg(target_id);

-- name: CountNotificationSettings :one
SELECT count(*) FROM notification_settings WHERE user_id = sqlc.arg(user_id);

-- name: ListNotificationSettingsForTargets :many
-- One query for the whole audience of a channel: its own rows and its server's.
SELECT * FROM notification_settings
WHERE target_id = ANY(sqlc.arg(target_ids)::uuid[])
ORDER BY user_id;

-- name: ListNotificationSettingsForUserTargets :many
SELECT * FROM notification_settings
WHERE user_id = sqlc.arg(user_id) AND target_id = ANY(sqlc.arg(target_ids)::uuid[]);
