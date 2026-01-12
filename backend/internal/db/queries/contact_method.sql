-- Contact method queries

-- name: ListContactMethodsByContact :many
SELECT * FROM contact_method
WHERE contact_id = $1
ORDER BY
    is_primary DESC,
    CASE type
        WHEN 'email' THEN 1
        WHEN 'phone' THEN 2
        WHEN 'whatsapp' THEN 3
        WHEN 'telegram' THEN 4
        WHEN 'signal' THEN 5
        WHEN 'discord' THEN 6
        WHEN 'twitter' THEN 7
        WHEN 'gchat' THEN 8
        ELSE 99
    END,
    created_at ASC;

-- name: CreateContactMethod :one
INSERT INTO contact_method (
    contact_id,
    type,
    value,
    value_normalized,
    is_primary
) VALUES (
    $1, $2, $3, $4, $5
) RETURNING *;

-- name: DeleteContactMethodsByContact :exec
DELETE FROM contact_method
WHERE contact_id = $1;

-- name: UpdateContactMethodValue :one
UPDATE contact_method cm
SET value = $2,
    value_normalized = $3,
    updated_at = NOW()
WHERE cm.id = $1
  AND EXISTS (
    SELECT 1 FROM contact c
    WHERE c.id = cm.contact_id
      AND c.deleted_at IS NULL
  )
RETURNING *;

-- name: FindMethodsByNormalizedValue :many
SELECT cm.*, c.full_name as contact_name
FROM contact_method cm
JOIN contact c ON c.id = cm.contact_id
WHERE cm.type = ANY($1::text[])
  AND cm.value_normalized = $2
  AND c.deleted_at IS NULL;
