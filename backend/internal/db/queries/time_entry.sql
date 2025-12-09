-- name: CreateTimeEntry :one
INSERT INTO time_entry (
    description,
    project,
    contact_id,
    start_time,
    end_time,
    duration_minutes
) VALUES (
    $1, $2, $3, $4, $5, $6
) RETURNING *;

-- name: GetTimeEntry :one
SELECT * FROM time_entry
WHERE id = $1 LIMIT 1;

-- name: ListTimeEntries :many
SELECT * FROM time_entry
ORDER BY start_time DESC
LIMIT $1 OFFSET $2;

-- name: ListTimeEntriesByDateRange :many
SELECT * FROM time_entry
WHERE start_time >= $1 AND start_time <= $2
ORDER BY start_time DESC;

-- name: ListTimeEntriesByContact :many
SELECT * FROM time_entry
WHERE contact_id = $1
ORDER BY start_time DESC;

-- name: GetRunningTimeEntry :one
SELECT * FROM time_entry
WHERE end_time IS NULL
ORDER BY start_time DESC
LIMIT 1;

-- name: UpdateTimeEntry :one
UPDATE time_entry
SET
    description = COALESCE($2, description),
    project = COALESCE($3, project),
    contact_id = COALESCE($4, contact_id),
    end_time = COALESCE($5, end_time),
    duration_minutes = COALESCE($6, duration_minutes),
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteTimeEntry :exec
DELETE FROM time_entry
WHERE id = $1;

-- name: GetTimeEntryStats :one
SELECT
    COUNT(*) as total_entries,
    COALESCE(SUM(duration_minutes), 0) as total_minutes,
    COALESCE(SUM(CASE WHEN DATE(start_time) = CURRENT_DATE THEN duration_minutes ELSE 0 END), 0) as today_minutes,
    COALESCE(SUM(CASE WHEN DATE(start_time) >= DATE_TRUNC('week', CURRENT_DATE) THEN duration_minutes ELSE 0 END), 0) as week_minutes,
    COALESCE(SUM(CASE WHEN DATE(start_time) >= DATE_TRUNC('month', CURRENT_DATE) THEN duration_minutes ELSE 0 END), 0) as month_minutes
FROM time_entry
WHERE end_time IS NOT NULL;
