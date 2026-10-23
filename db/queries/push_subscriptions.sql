-- Web Push subscriptions (internal/push).

-- name: UpsertPushSubscription :one
-- A browser that subscribes again (new keys, another account) keeps its row.
INSERT INTO push_subscriptions (id, user_id, endpoint, p256dh, auth)
VALUES (sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(endpoint), sqlc.arg(p256dh), sqlc.arg(auth))
ON CONFLICT (endpoint) DO UPDATE
SET user_id = EXCLUDED.user_id, p256dh = EXCLUDED.p256dh, auth = EXCLUDED.auth
RETURNING *;

-- name: DeletePushSubscription :execrows
DELETE FROM push_subscriptions WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);

-- name: DeletePushSubscriptionByEndpoint :exec
-- The push service said the browser is gone (404 or 410).
DELETE FROM push_subscriptions WHERE endpoint = sqlc.arg(endpoint);

-- name: ListPushSubscriptionsForUsers :many
SELECT * FROM push_subscriptions WHERE user_id = ANY(sqlc.arg(user_ids)::uuid[]) ORDER BY created_at;

-- name: TouchPushSubscription :exec
UPDATE push_subscriptions SET last_success_at = now() WHERE endpoint = sqlc.arg(endpoint);
