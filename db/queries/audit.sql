-- The moderation journal (internal/audit). The table is owned by that package;
-- nothing else reads or writes it through SQL.

-- name: RecordAuditEntry :one
INSERT INTO audit_log (workspace_id, actor_id, action, target_type, target_id, metadata)
VALUES (
    sqlc.arg(workspace_id),
    sqlc.arg(actor_id),
    sqlc.arg(action),
    sqlc.arg(target_type),
    sqlc.arg(target_id),
    sqlc.arg(metadata)
)
RETURNING *;

-- name: ListAuditEntries :many
-- One page of one server's journal, newest first. `before_created_at` and
-- `before_id` are the keyset cursor: both set for every page but the first.
SELECT * FROM audit_log
WHERE workspace_id = sqlc.arg(workspace_id)
  AND (sqlc.narg(action)::text IS NULL OR action = sqlc.narg(action)::text)
  AND (sqlc.narg(actor_id)::uuid IS NULL OR actor_id = sqlc.narg(actor_id)::uuid)
  AND (
      sqlc.narg(before_created_at)::timestamptz IS NULL
      OR (created_at, id) < (sqlc.narg(before_created_at)::timestamptz, sqlc.narg(before_id)::uuid)
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::integer;
