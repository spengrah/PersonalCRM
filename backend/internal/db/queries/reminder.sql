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
SELECT r.*,
       c.full_name as contact_name,
       cm.type as contact_primary_method_type,
       cm.value as contact_primary_method_value
FROM reminder r
LEFT JOIN contact c ON r.contact_id = c.id
LEFT JOIN LATERAL (
    SELECT type, value
    FROM contact_method
    WHERE contact_id = c.id
    ORDER BY
        CASE WHEN is_primary THEN 0 ELSE 1 END,
        CASE type
            WHEN 'email_personal' THEN 1
            WHEN 'email_work' THEN 2
            WHEN 'phone' THEN 3
            WHEN 'telegram' THEN 4
            WHEN 'signal' THEN 5
            WHEN 'discord' THEN 6
            WHEN 'twitter' THEN 7
            ELSE 8
        END,
        created_at ASC
    LIMIT 1
) cm ON TRUE
WHERE r.due_date <= $1 
  AND r.completed = FALSE 
  AND r.deleted_at IS NULL
  AND (c.deleted_at IS NULL OR r.contact_id IS NULL)
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
