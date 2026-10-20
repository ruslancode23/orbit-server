-- Devices are a person's browsers and apps. Only public keys live here: the
-- private halves never leave the device, so the server cannot read a chat.
-- name: CreateDevice :one
INSERT INTO devices (user_id, name, identity_key, signing_key)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetDevice :one
SELECT * FROM devices WHERE id = $1 AND revoked_at IS NULL;

-- name: GetUserDeviceByKeys :one
SELECT * FROM devices
WHERE user_id = $1 AND identity_key = $2 AND signing_key = $3 AND revoked_at IS NULL;

-- name: ListUserDevices :many
SELECT * FROM devices
WHERE user_id = $1 AND revoked_at IS NULL
ORDER BY created_at;

-- name: RevokeDevice :execrows
UPDATE devices SET revoked_at = now()
WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL;

-- name: TouchDevice :exec
UPDATE devices SET last_seen_at = now() WHERE id = $1;

-- One-time keys are claimed, never listed: a key is handed out once, and the
-- delete is what makes it atomic.
-- name: AddOneTimeKey :exec
INSERT INTO device_one_time_keys (device_id, key_id, key)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: ClaimOneTimeKey :one
DELETE FROM device_one_time_keys AS claimed
WHERE claimed.device_id = $1
  AND claimed.key_id = (
    SELECT pending.key_id FROM device_one_time_keys AS pending
    WHERE pending.device_id = $1
    ORDER BY pending.created_at, pending.key_id
    LIMIT 1
  )
RETURNING claimed.key_id, claimed.key;

-- name: CountOneTimeKeys :one
SELECT count(*) FROM device_one_time_keys WHERE device_id = $1;

-- A secret chat is a pair of devices; this table is the binding.
-- name: AddSecretChatDevice :exec
INSERT INTO secret_chat_devices (channel_id, user_id, device_id)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: ListSecretChatDevices :many
SELECT * FROM secret_chat_devices WHERE channel_id = $1 ORDER BY created_at;

-- name: IsSecretChatDevice :one
SELECT EXISTS (
    SELECT 1 FROM secret_chat_devices WHERE channel_id = $1 AND device_id = $2
);

-- The peer of a secret chat, as this device sees it: the other device, its
-- person and its public keys.
-- name: GetUserSecretChat :one
SELECT
    channels.id AS channel_id,
    channels.created_at AS channel_created_at,
    peer_user.id AS peer_user_id,
    peer_user.nickname AS peer_nickname,
    peer_user.is_guest AS peer_is_guest,
    peer_user.created_at AS peer_created_at,
    peer_device.id AS peer_device_id,
    peer_device.identity_key AS peer_identity_key,
    peer_device.signing_key AS peer_signing_key
FROM secret_chat_devices mine
    JOIN channels ON channels.id = mine.channel_id
    JOIN secret_chat_devices theirs
        ON theirs.channel_id = channels.id AND theirs.device_id <> mine.device_id
    JOIN users peer_user ON peer_user.id = theirs.user_id
    JOIN devices peer_device ON peer_device.id = theirs.device_id
WHERE channels.id = $1
  AND channels.kind = 'secret'
  AND mine.device_id = $2
  AND mine.user_id = $3;

-- name: ListUserSecretChats :many
SELECT
    channels.id AS channel_id,
    channels.created_at AS channel_created_at,
    peer_user.id AS peer_user_id,
    peer_user.nickname AS peer_nickname,
    peer_user.is_guest AS peer_is_guest,
    peer_user.created_at AS peer_created_at,
    peer_device.id AS peer_device_id,
    peer_device.identity_key AS peer_identity_key,
    peer_device.signing_key AS peer_signing_key
FROM secret_chat_devices mine
    JOIN channels ON channels.id = mine.channel_id
    JOIN secret_chat_devices theirs
        ON theirs.channel_id = channels.id AND theirs.device_id <> mine.device_id
    JOIN users peer_user ON peer_user.id = theirs.user_id
    JOIN devices peer_device ON peer_device.id = theirs.device_id
WHERE channels.kind = 'secret'
  AND mine.device_id = $1
  AND mine.user_id = $2
ORDER BY channels.created_at DESC;

-- Envelopes are opaque bytes in, opaque bytes out.
-- name: CreateSecretEnvelope :one
INSERT INTO secret_envelopes (channel_id, sender_device_id, recipient_device_id, type, body, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListPendingSecretEnvelopes :many
SELECT * FROM secret_envelopes
WHERE channel_id = $1
  AND recipient_device_id = $2
  AND expires_at > now()
ORDER BY created_at, id
LIMIT $3;

-- name: GetSecretEnvelope :one
SELECT * FROM secret_envelopes WHERE id = $1;

-- name: DeleteSecretEnvelope :exec
DELETE FROM secret_envelopes WHERE id = $1;

-- name: DeleteExpiredSecretEnvelopes :execrows
DELETE FROM secret_envelopes WHERE expires_at <= now();

-- name: CountUserDevices :one
SELECT count(*) FROM devices WHERE user_id = $1 AND revoked_at IS NULL;

-- name: DeviceSecretChatIDs :many
SELECT channel_id FROM secret_chat_devices WHERE device_id = $1;

-- name: DeleteDeviceEnvelopes :exec
DELETE FROM secret_envelopes WHERE sender_device_id = $1 OR recipient_device_id = $1;

-- name: DeleteDeviceSecretChatBindings :exec
DELETE FROM secret_chat_devices WHERE device_id = $1;

-- name: DeleteSecretChatChannel :exec
DELETE FROM channels WHERE id = $1 AND kind = 'secret';

-- Deleting a secret chat deletes the pair: the channel cascades to bindings,
-- envelopes and files. Only a device in the chat may do it.
-- name: LeaveSecretChat :execrows
DELETE FROM channels
WHERE id = $1
  AND kind = 'secret'
  AND EXISTS (
    SELECT 1 FROM secret_chat_devices
    WHERE channel_id = $1 AND device_id = $2 AND user_id = $3
  );
