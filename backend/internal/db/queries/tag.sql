-- Tag queries

-- name: GetTag :one
SELECT * FROM tag WHERE id = $1;

-- name: GetTagByName :one
SELECT * FROM tag WHERE name = $1;

-- name: ListTags :many
SELECT * FROM tag ORDER BY name ASC;

-- ListContactTagsWithLiveContact returns every contact_tag whose contact is NOT
-- soft-deleted (the `crm-admin --migrate-tags` source set). Deleted contacts are
-- skipped permanently — the assertion write path rejects a tombstoned subject
-- node anyway. Ordered deterministically so the migration processes rows in a
-- stable sequence.
-- name: ListContactTagsWithLiveContact :many
SELECT ct.contact_id, ct.tag_id, ct.created_at
FROM contact_tag ct
JOIN contact c ON c.id = ct.contact_id
WHERE c.deleted_at IS NULL
ORDER BY ct.contact_id ASC, ct.tag_id ASC;

-- CountContactTagsWithDeletedContact counts the contact_tag rows the migration
-- SKIPS because their contact is soft-deleted, so the `--migrate-tags` summary can
-- report the skip explicitly and the operator isn't misled when the migrated
-- count doesn't match the raw contact_tag table.
-- name: CountContactTagsWithDeletedContact :one
SELECT COUNT(*)
FROM contact_tag ct
JOIN contact c ON c.id = ct.contact_id
WHERE c.deleted_at IS NOT NULL;

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
