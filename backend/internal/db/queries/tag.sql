-- Tag queries

-- name: GetTag :one
SELECT * FROM tag WHERE id = $1;

-- name: GetTagByName :one
SELECT * FROM tag WHERE name = $1;

-- name: ListTags :many
SELECT * FROM tag ORDER BY name ASC;

-- name: CreateTag :one
INSERT INTO tag (name, color) VALUES ($1, $2) RETURNING *;

-- name: UpdateTag :one
UPDATE tag SET
  name = $2,
  color = $3
WHERE id = $1
RETURNING *;

-- name: DeleteTag :exec
DELETE FROM tag WHERE id = $1;

-- name: GetContactTags :many
SELECT t.* FROM tag t
JOIN contact_tag ct ON ct.tag_id = t.id
WHERE ct.contact_id = $1
ORDER BY t.name ASC;

-- name: AddContactTag :exec
INSERT INTO contact_tag (contact_id, tag_id) VALUES ($1, $2)
ON CONFLICT (contact_id, tag_id) DO NOTHING;

-- name: RemoveContactTag :exec
DELETE FROM contact_tag 
WHERE contact_id = $1 AND tag_id = $2;
