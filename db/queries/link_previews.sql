-- Link previews (internal/linkpreview).

-- name: GetLinkPreview :one
SELECT * FROM link_previews WHERE url = sqlc.arg(url);

-- name: UpsertLinkPreview :one
INSERT INTO link_previews (id, url, title, description, site_name, image_url, status, fetched_at)
VALUES (
    sqlc.arg(id), sqlc.arg(url), sqlc.narg(title), sqlc.narg(description),
    sqlc.narg(site_name), sqlc.narg(image_url), sqlc.arg(status), now()
)
ON CONFLICT (url) DO UPDATE
SET title = EXCLUDED.title,
    description = EXCLUDED.description,
    site_name = EXCLUDED.site_name,
    image_url = EXCLUDED.image_url,
    status = EXCLUDED.status,
    fetched_at = now()
RETURNING *;

-- name: AttachMessagePreview :exec
INSERT INTO message_previews (message_id, preview_id)
VALUES (sqlc.arg(message_id), sqlc.arg(preview_id))
ON CONFLICT (message_id) DO UPDATE SET preview_id = EXCLUDED.preview_id;

-- name: DeleteMessagePreview :exec
DELETE FROM message_previews WHERE message_id = sqlc.arg(message_id);

-- name: ListPreviewsForMessages :many
SELECT message_previews.message_id, link_previews.*
FROM message_previews
    JOIN link_previews ON link_previews.id = message_previews.preview_id
WHERE message_previews.message_id = ANY(sqlc.arg(message_ids)::uuid[]);

-- name: GetLinkPreviewByID :one
SELECT * FROM link_previews WHERE id = sqlc.arg(id);
