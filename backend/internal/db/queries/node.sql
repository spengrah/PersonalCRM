-- Node registry queries (graph foundation).

-- name: CreateNode :one
-- Caller supplies the id (for persons, id == contact.id); node has no default.
INSERT INTO node (id, type, canonical_label)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetNode :one
SELECT * FROM node WHERE id = $1 AND deleted_at IS NULL;

-- name: GetNodeIncludingDeleted :one
SELECT * FROM node WHERE id = $1;

-- name: SoftDeleteNode :exec
UPDATE node SET deleted_at = NOW() WHERE id = $1;

-- name: SetNodeMergedInto :exec
-- Records the merge alias (loser → winner) and tombstones the loser node.
UPDATE node SET merged_into = $2, deleted_at = NOW() WHERE id = $1;

-- name: UpdateNodeCanonicalLabel :exec
UPDATE node SET canonical_label = $2 WHERE id = $1;
