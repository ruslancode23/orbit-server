-- +goose Up
-- The moderation journal (SCRUM-162, internal/audit): who banned, kicked,
-- granted a role, deleted a channel or a message, made or revoked an invite,
-- or handed the server over. One row is one action; the row outlives the
-- action's target, so target_id is not a foreign key (it names a user, a role,
-- a channel, a message or an invite).
--
-- actor_id is a foreign key with ON DELETE SET NULL: deleting an account keeps
-- its record, and the reader sees «deleted account» instead of a name. The
-- workspace cascades: a deleted server takes its journal with it.
--
-- metadata is jsonb for the little the row needs beyond its target (the
-- channel of a deleted message, the id of the role), and it is deliberately
-- not a place for message text, invite codes, tokens or other secrets: the
-- journal is read by everyone who may manage the server.
CREATE TABLE audit_log (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    actor_id     uuid        REFERENCES users (id) ON DELETE SET NULL,
    action       text        NOT NULL,
    target_type  text        NOT NULL,
    target_id    uuid,
    metadata     jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT audit_log_action_not_empty CHECK (action <> ''),
    CONSTRAINT audit_log_target_type_not_empty CHECK (target_type <> '')
);

-- The page cursor: newest first inside one server. The id direction matches
-- the cursor's (created_at DESC, id DESC), so a page is one index range scan
-- and not a sort over the whole journal.
CREATE INDEX audit_log_workspace_cursor_idx ON audit_log (workspace_id, created_at DESC, id DESC);
-- The «what happened on this server» filter.
CREATE INDEX audit_log_workspace_action_idx ON audit_log (workspace_id, action);
-- The «what did this moderator do» filter.
CREATE INDEX audit_log_workspace_actor_idx ON audit_log (workspace_id, actor_id);

-- +goose Down
DROP INDEX audit_log_workspace_actor_idx;
DROP INDEX audit_log_workspace_action_idx;
DROP INDEX audit_log_workspace_cursor_idx;
DROP TABLE audit_log;
