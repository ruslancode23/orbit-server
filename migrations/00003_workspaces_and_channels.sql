-- +goose Up
-- Servers (workspace) with roles (FR-02), their channels, invites and bans (FR-03).
CREATE TYPE workspace_role AS ENUM ('owner', 'admin', 'member');

CREATE TABLE workspaces (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    owner_id   uuid        NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT workspaces_name_length CHECK (char_length(name) BETWEEN 1 AND 64)
);

CREATE TABLE workspace_members (
    workspace_id uuid           NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    user_id      uuid           NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role         workspace_role NOT NULL DEFAULT 'member',
    joined_at    timestamptz    NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, user_id)
);

-- "Which servers am I in" on every page load.
CREATE INDEX workspace_members_user_id_idx ON workspace_members (user_id);

CREATE TABLE bans (
    workspace_id uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    banned_by    uuid        REFERENCES users (id) ON DELETE SET NULL,
    reason       text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, user_id)
);

-- A direct or group conversation is a channel without a workspace, so messages,
-- attachments and read cursors work for it through the same tables (task 2.10 adds its members).
CREATE TYPE channel_kind AS ENUM ('text', 'direct', 'group');

CREATE TABLE channels (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid         REFERENCES workspaces (id) ON DELETE CASCADE,
    kind         channel_kind NOT NULL DEFAULT 'text',
    name         text,
    topic        text,
    position     integer      NOT NULL DEFAULT 0,
    created_at   timestamptz  NOT NULL DEFAULT now(),
    updated_at   timestamptz  NOT NULL DEFAULT now(),
    -- A channel of a server is named and belongs to it; a conversation has neither.
    CONSTRAINT channels_text_in_workspace CHECK (
        (kind = 'text' AND workspace_id IS NOT NULL AND name IS NOT NULL)
        OR (kind <> 'text' AND workspace_id IS NULL AND name IS NULL)
    ),
    CONSTRAINT channels_name_length CHECK (name IS NULL OR char_length(name) BETWEEN 1 AND 64)
);

-- The sidebar reads the channels of one server in their order.
CREATE INDEX channels_workspace_id_position_idx ON channels (workspace_id, position, created_at)
    WHERE workspace_id IS NOT NULL;
CREATE UNIQUE INDEX channels_workspace_name_key ON channels (workspace_id, lower(name))
    WHERE workspace_id IS NOT NULL;

CREATE TABLE invites (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    code         text        NOT NULL,
    created_by   uuid        REFERENCES users (id) ON DELETE SET NULL,
    expires_at   timestamptz,
    max_uses     integer,
    uses         integer     NOT NULL DEFAULT 0,
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT invites_max_uses_positive CHECK (max_uses IS NULL OR max_uses > 0)
);

CREATE UNIQUE INDEX invites_code_key ON invites (code);
CREATE INDEX invites_workspace_id_idx ON invites (workspace_id);

-- +goose Down
DROP TABLE invites;
DROP TABLE channels;
DROP TYPE channel_kind;
DROP TABLE bans;
DROP TABLE workspace_members;
DROP TABLE workspaces;
DROP TYPE workspace_role;
