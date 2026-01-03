-- Contact queries

-- name: GetContact :one
SELECT * FROM contact 
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListContacts :many
SELECT * FROM contact 
WHERE deleted_at IS NULL
LIMIT $1 OFFSET $2;

-- name: SearchContacts :many
SELECT c.* FROM contact c
LEFT JOIN (
  SELECT contact_id, string_agg(value, ' ') AS method_values
  FROM contact_method
  GROUP BY contact_id
) cm ON cm.contact_id = c.id
WHERE c.deleted_at IS NULL
  AND to_tsvector('english', c.full_name || ' ' || COALESCE(cm.method_values, '')) @@ plainto_tsquery('english', $1)
ORDER BY ts_rank(
  to_tsvector('english', c.full_name || ' ' || COALESCE(cm.method_values, '')),
  plainto_tsquery('english', $1)
) DESC
LIMIT $2 OFFSET $3;

-- name: CreateContact :one
INSERT INTO contact (
  full_name, location, birthday, how_met, cadence, last_contacted, profile_photo, created_at
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8
) RETURNING *;

-- name: UpdateContact :one
UPDATE contact SET
  full_name = $2,
  location = $3,
  birthday = $4,
  how_met = $5,
  cadence = $6,
  profile_photo = $7,
  updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: UpdateContactLastContacted :exec
UPDATE contact SET
  last_contacted = $2,
  updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: SoftDeleteContact :exec
UPDATE contact SET
  deleted_at = NOW(),
  updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: HardDeleteContact :exec
DELETE FROM contact WHERE id = $1;

-- name: CountContacts :one
SELECT COUNT(*) FROM contact WHERE deleted_at IS NULL;
