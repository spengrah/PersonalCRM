-- name: CreateReminder :one
INSERT INTO reminder (
    contact_id,
    title,
    description,
    due_date
) VALUES (
    $1, $2, $3, $4
) RETURNING *;

-- name: GetReminder :one
SELECT * FROM reminder
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListReminders :many
SELECT * FROM reminder
WHERE deleted_at IS NULL
ORDER BY due_date ASC
LIMIT $1 OFFSET $2;

-- name: ListDueReminders :many
SELECT r.*, c.full_name as contact_name, c.email as contact_email
FROM reminder r
JOIN contact c ON r.contact_id = c.id
WHERE r.due_date <= $1 
  AND r.completed = FALSE 
  AND r.deleted_at IS NULL
  AND c.deleted_at IS NULL
ORDER BY r.due_date ASC;

-- name: ListRemindersByContact :many
SELECT * FROM reminder
WHERE contact_id = $1 AND deleted_at IS NULL
ORDER BY due_date DESC;

-- name: CompleteReminder :one
UPDATE reminder
SET completed = TRUE, completed_at = NOW()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: UpdateReminder :one
UPDATE reminder
SET title = COALESCE($2, title),
    description = COALESCE($3, description),
    due_date = COALESCE($4, due_date)
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteReminder :exec
UPDATE reminder
SET deleted_at = NOW()
WHERE id = $1;

-- name: HardDeleteReminder :exec
DELETE FROM reminder
WHERE id = $1;

-- name: CountReminders :one
SELECT COUNT(*) FROM reminder
WHERE deleted_at IS NULL;

-- name: CountDueReminders :one
SELECT COUNT(*) FROM reminder
WHERE due_date <= $1 
  AND completed = FALSE 
  AND deleted_at IS NULL;