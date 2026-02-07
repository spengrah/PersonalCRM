-- Interaction queries

-- name: GetInteraction :one
SELECT * FROM interaction WHERE id = $1 AND deleted_at IS NULL;

-- name: ListContactInteractions :many
SELECT * FROM interaction
WHERE contact_id = $1 AND deleted_at IS NULL
ORDER BY occurred_at DESC
LIMIT $2 OFFSET $3;

-- name: CreateInteraction :one
INSERT INTO interaction (contact_id, source, source_ref, occurred_at, description)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: SoftDeleteInteraction :exec
UPDATE interaction SET deleted_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: CountContactInteractions :one
SELECT COUNT(*) FROM interaction
WHERE contact_id = $1 AND deleted_at IS NULL;

-- name: FindInteractionBySourceRef :one
-- Find an existing interaction by contact, source, and source_ref (for deduplication)
SELECT * FROM interaction
WHERE contact_id = $1 AND source = $2 AND source_ref = $3 AND deleted_at IS NULL
LIMIT 1;

-- name: FindInteractionInWindow :one
-- Find an existing manual interaction within a time window (for manual deduplication)
SELECT * FROM interaction
WHERE contact_id = $1
  AND source = $4
  AND deleted_at IS NULL
  AND occurred_at BETWEEN $2 AND $3
ORDER BY occurred_at DESC
LIMIT 1;
