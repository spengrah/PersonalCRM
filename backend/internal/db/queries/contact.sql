-- Contact queries

-- name: GetContact :one
SELECT * FROM contact 
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetContactByEmail :one
SELECT * FROM contact 
WHERE LOWER(email) = LOWER($1) AND deleted_at IS NULL;

-- name: ListContacts :many
SELECT * FROM contact 
WHERE deleted_at IS NULL
ORDER BY full_name ASC
LIMIT $1 OFFSET $2;

-- name: SearchContacts :many
SELECT * FROM contact 
WHERE deleted_at IS NULL 
  AND (
    full_name ILIKE '%' || $1 || '%' 
    OR email ILIKE '%' || $1 || '%'
  )
ORDER BY full_name ASC
LIMIT $2 OFFSET $3;

-- name: CreateContact :one
INSERT INTO contact (
  full_name, email, phone, location, birthday, how_met, cadence, last_contacted, profile_photo
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8, $9
) RETURNING *;

-- name: UpdateContact :one
UPDATE contact SET
  full_name = $2,
  email = $3,
  phone = $4,
  location = $5,
  birthday = $6,
  how_met = $7,
  cadence = $8,
  profile_photo = $9,
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
