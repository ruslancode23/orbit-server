-- Bug reports (internal/feedback). The module only writes them; the team reads
-- the table directly, so there is no list query here yet.

-- name: CreateBugReport :one
INSERT INTO bug_reports (
    id, user_id, description, steps, app_version, user_agent, route,
    screenshot_key, screenshot_type, screenshot_bytes
) VALUES (
    sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(description), sqlc.arg(steps),
    sqlc.arg(app_version), sqlc.arg(user_agent), sqlc.arg(route),
    sqlc.narg(screenshot_key), sqlc.narg(screenshot_type), sqlc.narg(screenshot_bytes)
)
RETURNING *;
