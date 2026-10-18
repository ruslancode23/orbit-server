-- name: CreateAttachment :one
INSERT INTO attachments (uploader_id, storage_key, file_name, mime_type, size_bytes, width, height)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- An attachment is bound to its message only by the user that uploaded it.
-- name: AttachToMessage :one
UPDATE attachments
SET message_id = $2
WHERE id = $1 AND uploader_id = $3 AND message_id IS NULL
RETURNING *;

-- name: ListMessageAttachments :many
SELECT * FROM attachments
WHERE message_id = ANY($1::uuid[])
ORDER BY created_at;

-- name: GetAttachment :one
SELECT * FROM attachments WHERE id = $1;

-- Files whose message never arrived, for the cleanup job (task 2.9).
-- name: ListOrphanAttachments :many
SELECT * FROM attachments
WHERE message_id IS NULL AND created_at < $1
ORDER BY created_at
LIMIT $2;

-- name: DeleteAttachment :exec
DELETE FROM attachments WHERE id = $1;

-- name: DetachOrphanedAttachment :exec
DELETE FROM attachments WHERE id = $1 AND message_id IS NULL;
