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
WHERE to_tsvector('english', body) @@ plainto_tsquery('english', $1)
ORDER BY ts_rank(
  to_tsvector('english', body),
  plainto_tsquery('english', $1)
) DESC, created_at DESC
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

-- name: GetContactNoteByCategory :one
-- Get a single note for a contact by category (e.g., 'notepad')
SELECT * FROM note
WHERE contact_id = $1 AND category = $2;

-- name: DeleteContactNoteByCategory :exec
-- Delete a note for a contact by category
DELETE FROM note
WHERE contact_id = $1 AND category = $2;

-- name: CreateNoteWithTimestamp :one
-- Create a note with a specific created_at timestamp (for migrations)
INSERT INTO note (contact_id, body, category, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: UpsertContactNoteByCategory :one
-- Insert or update a note for a contact by category (atomic operation for concurrent safety)
-- Note: This uses the unique index on (contact_id) WHERE category = 'notepad'
INSERT INTO note (contact_id, body, category)
VALUES ($1, $2, $3)
ON CONFLICT (contact_id) WHERE category = 'notepad'
DO UPDATE SET body = EXCLUDED.body, updated_at = NOW()
RETURNING *;
