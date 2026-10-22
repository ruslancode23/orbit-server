-- +goose Up
-- Server roles (ROLE-2): a member holds any number of roles, permissions add
-- up, roles are ordered, channels override them per role. The bits are those
-- of internal/perms; the order is part of the data and is never changed:
--   administrator 1, manageServer 2, manageRoles 4, manageChannels 8,
--   createChannels 16, kickMembers 32, banMembers 64, manageMessages 128,
--   createInvite 256, attachFiles 512, viewChannel 1024, sendMessages 2048,
--   speak 4096.

CREATE TABLE server_roles (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    name         text        NOT NULL,
    -- An accent token of the client palette, the same as a profile's; NULL is
    -- no colour.
    color        text,
    -- Shown as its own group in the member column.
    hoist        boolean     NOT NULL DEFAULT false,
    -- Higher is more senior. @everyone is 0 and the only one there.
    position     integer     NOT NULL,
    permissions  bigint      NOT NULL DEFAULT 0,
    -- @everyone: every member holds it without being granted it.
    is_default   boolean     NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT server_roles_name_length CHECK (char_length(name) BETWEEN 1 AND 32),
    CONSTRAINT server_roles_permissions_known CHECK (permissions >= 0 AND permissions < 8192),
    CONSTRAINT server_roles_default_at_zero CHECK (is_default = (position = 0))
);
CREATE UNIQUE INDEX server_roles_one_default ON server_roles (workspace_id) WHERE is_default;
CREATE UNIQUE INDEX server_roles_position ON server_roles (workspace_id, position);

CREATE TABLE server_member_roles (
    workspace_id uuid NOT NULL,
    user_id      uuid NOT NULL,
    role_id      uuid NOT NULL REFERENCES server_roles (id) ON DELETE CASCADE,
    PRIMARY KEY (workspace_id, user_id, role_id),
    FOREIGN KEY (workspace_id, user_id)
        REFERENCES workspace_members (workspace_id, user_id) ON DELETE CASCADE
);
CREATE INDEX server_member_roles_role_idx ON server_member_roles (role_id);

CREATE TABLE channel_role_overrides (
    channel_id uuid   NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    role_id    uuid   NOT NULL REFERENCES server_roles (id) ON DELETE CASCADE,
    allow      bigint NOT NULL DEFAULT 0,
    deny       bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (channel_id, role_id),
    CONSTRAINT channel_role_overrides_known CHECK (allow >= 0 AND allow < 8192 AND deny >= 0 AND deny < 8192),
    CONSTRAINT channel_role_overrides_disjoint CHECK (allow & deny = 0)
);

-- @everyone of every server: view, write and speak, plus what the server let
-- its members do. Nobody gains or loses anything in the move.
INSERT INTO server_roles (workspace_id, name, position, permissions, is_default)
SELECT id, '@everyone', 0,
       1024 | 2048 | 4096
       | CASE WHEN members_can_invite THEN 256 ELSE 0 END
       | CASE WHEN members_can_create_channels THEN 16 ELSE 0 END
       | CASE WHEN members_can_attach_files THEN 512 ELSE 0 END,
       true
FROM workspaces;

-- Admins become members holding «Администратор», which grants everything.
INSERT INTO server_roles (workspace_id, name, position, permissions, hoist)
SELECT DISTINCT workspace_id, 'Администратор', 1, 1, true
FROM workspace_members
WHERE role = 'admin';

INSERT INTO server_member_roles (workspace_id, user_id, role_id)
SELECT members.workspace_id, members.user_id, roles.id
FROM workspace_members members
    JOIN server_roles roles
        ON roles.workspace_id = members.workspace_id AND roles.position = 1
WHERE members.role = 'admin';

UPDATE workspace_members SET role = 'member' WHERE role = 'admin';

-- «Only admins write» becomes an @everyone override in that channel.
INSERT INTO channel_role_overrides (channel_id, role_id, deny)
SELECT channels.id, roles.id, 2048 | 4096
FROM channels
    JOIN server_roles roles
        ON roles.workspace_id = channels.workspace_id AND roles.is_default
WHERE channels.is_read_only;

ALTER TABLE channels DROP COLUMN is_read_only;
ALTER TABLE workspaces
    DROP COLUMN members_can_invite,
    DROP COLUMN members_can_create_channels,
    DROP COLUMN members_can_attach_files;

-- +goose Down
ALTER TABLE workspaces
    ADD COLUMN members_can_invite          boolean NOT NULL DEFAULT false,
    ADD COLUMN members_can_create_channels boolean NOT NULL DEFAULT false,
    ADD COLUMN members_can_attach_files    boolean NOT NULL DEFAULT true;
ALTER TABLE channels ADD COLUMN is_read_only boolean NOT NULL DEFAULT false;

UPDATE workspaces
SET members_can_invite          = roles.permissions & 256 <> 0,
    members_can_create_channels = roles.permissions & (16 | 8) <> 0,
    members_can_attach_files    = roles.permissions & 512 <> 0
FROM server_roles roles
WHERE roles.workspace_id = workspaces.id AND roles.is_default;

UPDATE channels
SET is_read_only = true
FROM channel_role_overrides overrides
    JOIN server_roles roles ON roles.id = overrides.role_id AND roles.is_default
WHERE overrides.channel_id = channels.id AND overrides.deny & 2048 <> 0;

-- Anyone holding a role with administrator or a management permission was an
-- admin before; the old model has nothing finer.
UPDATE workspace_members
SET role = 'admin'
WHERE role = 'member'
  AND EXISTS (
      SELECT 1
      FROM server_member_roles held
          JOIN server_roles roles ON roles.id = held.role_id
      WHERE held.workspace_id = workspace_members.workspace_id
        AND held.user_id = workspace_members.user_id
        AND roles.permissions & (1 | 2 | 4 | 8 | 32 | 64) <> 0
  );

DROP TABLE channel_role_overrides;
DROP TABLE server_member_roles;
DROP TABLE server_roles;
