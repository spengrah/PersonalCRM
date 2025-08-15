-- Note queries

-- name: GetNote :one
SELECT * FROM note WHERE id = $1;

-- name: ListContactNotes :many
SELECT * FROM note 
WHERE contact_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: SearchNotes :many
SELECT * FROM note 
WHERE body ILIKE '%' || $1 || '%'
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: CreateNote :one
INSERT INTO note (contact_id, body, category) 
VALUES ($1, $2, $3) 
RETURNING *;

-- name: UpdateNote :one
UPDATE note SET
  body = $2,
  category = $3,
  updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteNote :exec
DELETE FROM note WHERE id = $1;

-- name: CountContactNotes :one
SELECT COUNT(*) FROM note WHERE contact_id = $1;
