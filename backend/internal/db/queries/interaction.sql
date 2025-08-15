-- Interaction queries

-- name: GetInteraction :one
SELECT * FROM interaction WHERE id = $1;

-- name: ListContactInteractions :many
SELECT * FROM interaction 
WHERE contact_id = $1
ORDER BY interaction_date DESC
LIMIT $2 OFFSET $3;

-- name: ListRecentInteractions :many
SELECT i.*, c.full_name as contact_name
FROM interaction i
JOIN contact c ON c.id = i.contact_id
WHERE c.deleted_at IS NULL
ORDER BY i.interaction_date DESC
LIMIT $1;

-- name: CreateInteraction :one
INSERT INTO interaction (contact_id, type, description, interaction_date) 
VALUES ($1, $2, $3, $4) 
RETURNING *;

-- name: UpdateInteraction :one
UPDATE interaction SET
  type = $2,
  description = $3,
  interaction_date = $4
WHERE id = $1
RETURNING *;

-- name: DeleteInteraction :exec
DELETE FROM interaction WHERE id = $1;

-- name: CountContactInteractions :one
SELECT COUNT(*) FROM interaction WHERE contact_id = $1;
