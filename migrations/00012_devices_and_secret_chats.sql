-- +goose Up
-- Devices, their one-time keys, secret chats and ciphertext envelopes.
--
-- A secret chat is a pair of devices, not a pair of accounts: the keys live on
-- the devices, so the same person on a laptop and a phone has two secret chats
-- with the same friend. The server keeps only public keys and opaque envelopes.

-- A conversation may now be secret as well. A secret chat has two participants
-- and no name, exactly like a direct one.
ALTER TABLE channels DROP CONSTRAINT channels_server_channel_in_workspace;
ALTER TABLE channels ADD CONSTRAINT channels_server_channel_in_workspace CHECK (
    (kind IN ('text', 'voice') AND workspace_id IS NOT NULL AND name IS NOT NULL)
    OR (kind IN ('direct', 'group', 'secret') AND workspace_id IS NULL AND name IS NULL)
);

CREATE TABLE devices (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- What the person sees in settings: "Chrome on macOS".
    name         text        NOT NULL DEFAULT '',
    -- Olm identity keys: curve25519 for the key exchange, ed25519 for signing.
    -- Public keys only; the private halves never leave the device.
    identity_key text        NOT NULL,
    signing_key  text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz,
    CONSTRAINT devices_name_length CHECK (char_length(name) <= 64)
);

-- A device proves it holds the signing key when it registers; the same keys
-- cannot be registered twice.
CREATE UNIQUE INDEX devices_user_keys_key ON devices (user_id, identity_key, signing_key);
CREATE INDEX devices_user_id_idx ON devices (user_id) WHERE revoked_at IS NULL;

CREATE TABLE device_one_time_keys (
    device_id  uuid        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    -- The client's own id for the key, so a re-upload is harmless.
    key_id     text        NOT NULL,
    key        text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (device_id, key_id),
    CONSTRAINT device_one_time_keys_key_id_length CHECK (char_length(key_id) <= 64)
);

-- Which device of which account takes part in a secret chat. One device per
-- person: a chat is a pair of devices, and a group is out of scope.
CREATE TABLE secret_chat_devices (
    channel_id uuid        NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    device_id  uuid        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (channel_id, device_id)
);

CREATE UNIQUE INDEX secret_chat_devices_channel_user_key ON secret_chat_devices (channel_id, user_id);
CREATE INDEX secret_chat_devices_device_id_idx ON secret_chat_devices (device_id);

-- One undelivered ciphertext. The server cannot read it and does not keep it
-- after it is delivered: acknowledging an envelope deletes it.
CREATE TABLE secret_envelopes (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    channel_id          uuid        NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    sender_device_id    uuid        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    recipient_device_id uuid        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    -- 0 is the first message of an Olm session (it carries the pre-key), 1 is a
    -- message of a session already established.
    type                smallint    NOT NULL,
    body                bytea       NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    -- A client that never comes back leaves the envelope to expire, never to
    -- accumulate. The longest timer a message can have is one week.
    expires_at          timestamptz NOT NULL,
    CONSTRAINT secret_envelopes_type_check CHECK (type IN (0, 1)),
    CONSTRAINT secret_envelopes_size_check CHECK (octet_length(body) <= 65536),
    CONSTRAINT secret_envelopes_expiry_check CHECK (expires_at > created_at)
);

CREATE INDEX secret_envelopes_recipient_idx
    ON secret_envelopes (recipient_device_id, channel_id, created_at);
CREATE INDEX secret_envelopes_expires_at_idx ON secret_envelopes (expires_at);

-- A file of a secret chat is encrypted on the client: the stored bytes are
-- ciphertext, the name is opaque and the type is whatever the blob sniffs as.
-- The channel lets a recipient download it; expires_at makes the timer delete
-- it from the server too, not only from the sender's screen.
ALTER TABLE attachments ADD COLUMN channel_id uuid REFERENCES channels (id) ON DELETE CASCADE;
ALTER TABLE attachments ADD COLUMN expires_at timestamptz;

UPDATE attachments
SET channel_id = messages.channel_id
FROM messages
WHERE attachments.message_id = messages.id;

CREATE INDEX attachments_channel_id_idx ON attachments (channel_id) WHERE channel_id IS NOT NULL;
CREATE INDEX attachments_expires_at_idx ON attachments (expires_at) WHERE expires_at IS NOT NULL;

-- +goose Down
DROP INDEX attachments_expires_at_idx;
DROP INDEX attachments_channel_id_idx;
ALTER TABLE attachments DROP COLUMN expires_at;
ALTER TABLE attachments DROP COLUMN channel_id;
DROP TABLE secret_envelopes;
DROP TABLE secret_chat_devices;
DROP TABLE device_one_time_keys;
DROP TABLE devices;
ALTER TABLE channels DROP CONSTRAINT channels_server_channel_in_workspace;
ALTER TABLE channels ADD CONSTRAINT channels_server_channel_in_workspace CHECK (
    (kind IN ('text', 'voice') AND workspace_id IS NOT NULL AND name IS NOT NULL)
    OR (kind IN ('direct', 'group') AND workspace_id IS NULL AND name IS NULL)
);
