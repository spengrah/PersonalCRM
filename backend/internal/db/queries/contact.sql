-- Contact queries

-- name: GetContact :one
SELECT * FROM contact 
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListContacts :many
SELECT * FROM contact 
WHERE deleted_at IS NULL
LIMIT $1 OFFSET $2;

-- name: ListContactsSorted :many
SELECT * FROM contact
WHERE deleted_at IS NULL
ORDER BY
  CASE WHEN sqlc.arg(sort_field) = 'name' AND sqlc.arg(sort_order) = 'asc' THEN full_name END ASC,
  CASE WHEN sqlc.arg(sort_field) = 'name' AND sqlc.arg(sort_order) = 'desc' THEN full_name END DESC,
  CASE WHEN sqlc.arg(sort_field) = 'location' AND sqlc.arg(sort_order) = 'asc' THEN COALESCE(location, '') END ASC,
  CASE WHEN sqlc.arg(sort_field) = 'location' AND sqlc.arg(sort_order) = 'desc' THEN COALESCE(location, '') END DESC,
  CASE WHEN sqlc.arg(sort_field) = 'birthday' AND sqlc.arg(sort_order) = 'asc' THEN birthday END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'birthday' AND sqlc.arg(sort_order) = 'desc' THEN birthday END DESC NULLS FIRST,
  CASE WHEN sqlc.arg(sort_field) = 'last_contacted' AND sqlc.arg(sort_order) = 'asc' THEN last_contacted END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'last_contacted' AND sqlc.arg(sort_order) = 'desc' THEN last_contacted END DESC NULLS FIRST
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

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

-- name: SearchContactsSorted :many
SELECT c.* FROM contact c
LEFT JOIN (
  SELECT contact_id, string_agg(value, ' ') AS method_values
  FROM contact_method
  GROUP BY contact_id
) cm ON cm.contact_id = c.id
WHERE c.deleted_at IS NULL
  AND to_tsvector('english', c.full_name || ' ' || COALESCE(cm.method_values, '')) @@ plainto_tsquery('english', sqlc.arg(search_query))
ORDER BY
  CASE WHEN sqlc.arg(sort_field) = 'name' AND sqlc.arg(sort_order) = 'asc' THEN c.full_name END ASC,
  CASE WHEN sqlc.arg(sort_field) = 'name' AND sqlc.arg(sort_order) = 'desc' THEN c.full_name END DESC,
  CASE WHEN sqlc.arg(sort_field) = 'location' AND sqlc.arg(sort_order) = 'asc' THEN COALESCE(c.location, '') END ASC,
  CASE WHEN sqlc.arg(sort_field) = 'location' AND sqlc.arg(sort_order) = 'desc' THEN COALESCE(c.location, '') END DESC,
  CASE WHEN sqlc.arg(sort_field) = 'birthday' AND sqlc.arg(sort_order) = 'asc' THEN c.birthday END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'birthday' AND sqlc.arg(sort_order) = 'desc' THEN c.birthday END DESC NULLS FIRST,
  CASE WHEN sqlc.arg(sort_field) = 'last_contacted' AND sqlc.arg(sort_order) = 'asc' THEN c.last_contacted END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort_field) = 'last_contacted' AND sqlc.arg(sort_order) = 'desc' THEN c.last_contacted END DESC NULLS FIRST
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CreateContact :one
INSERT INTO contact (
  full_name, location, birthday, how_met, cadence, last_contacted, profile_photo, notes, created_at
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8, $9
) RETURNING *;

-- name: UpdateContact :one
UPDATE contact SET
  full_name = $2,
  location = $3,
  birthday = $4,
  how_met = $5,
  cadence = $6,
  profile_photo = $7,
  notes = $8,
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

-- name: CountSearchContacts :one
SELECT COUNT(*) FROM contact c
LEFT JOIN (
  SELECT contact_id, string_agg(value, ' ') AS method_values
  FROM contact_method
  GROUP BY contact_id
) cm ON cm.contact_id = c.id
WHERE c.deleted_at IS NULL
  AND to_tsvector('english', c.full_name || ' ' || COALESCE(cm.method_values, '')) @@ plainto_tsquery('english', $1);

-- name: FindSimilarContacts :many
SELECT
  c.id,
  c.full_name,
  similarity(c.full_name, sqlc.arg(search_name)::text) as name_similarity,
  COALESCE(
    json_agg(
      json_build_object(
        'type', cm.type,
        'value', cm.value
      )
    ) FILTER (WHERE cm.id IS NOT NULL),
    '[]'
  )::jsonb as methods_json
FROM contact c
LEFT JOIN contact_method cm ON c.id = cm.contact_id
WHERE c.deleted_at IS NULL
  AND similarity(c.full_name, sqlc.arg(search_name)::text) > sqlc.arg(threshold)::real
GROUP BY c.id, c.full_name
ORDER BY similarity(c.full_name, sqlc.arg(search_name)::text) DESC
LIMIT sqlc.arg(result_limit);
