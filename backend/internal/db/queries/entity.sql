-- Entity subtype queries (graph foundation).
--
-- Entity rows have no deleted_at of their own: liveness flows from the parent
-- node's tombstone (a merge or soft-delete sets node.deleted_at). So the live
-- reads join node and filter node.deleted_at IS NULL; an entity whose node has
-- been merged/soft-deleted drops from these reads.

-- name: CreateEntity :one
INSERT INTO entity (node_id, subtype, normalized_name, external_ref, detail)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetEntity :one
SELECT entity.* FROM entity
JOIN node ON node.id = entity.node_id
WHERE entity.node_id = $1 AND node.deleted_at IS NULL;

-- name: FindEntityBySubtypeName :one
-- Entity-resolution dedup lookup against the (subtype, normalized_name) unique;
-- excludes entities whose node has been merged/soft-deleted.
SELECT entity.* FROM entity
JOIN node ON node.id = entity.node_id
WHERE entity.subtype = $1 AND entity.normalized_name = $2 AND node.deleted_at IS NULL;

-- name: UpdateEntityDetail :exec
-- Merge-patches the per-instance detail JSONB (e.g. a tag color edit) using the
-- || concatenation operator so other keys are preserved, not overwritten.
UPDATE entity SET detail = detail || $2 WHERE node_id = $1;
