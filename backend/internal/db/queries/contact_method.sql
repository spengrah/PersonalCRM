-- Contact method queries

-- name: ListContactMethodsByContact :many
SELECT * FROM contact_method
WHERE contact_id = $1
ORDER BY is_primary DESC, created_at ASC;

-- name: CreateContactMethod :one
INSERT INTO contact_method (
    contact_id,
    type,
    value,
    is_primary
) VALUES (
    $1, $2, $3, $4
) RETURNING *;

-- name: DeleteContactMethodsByContact :exec
DELETE FROM contact_method
WHERE contact_id = $1;
