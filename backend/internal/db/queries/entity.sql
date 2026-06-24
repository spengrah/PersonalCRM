-- Entity subtype queries (SP1 graph foundation).

-- name: CreateEntity :one
INSERT INTO entity (node_id, subtype, normalized_name, external_ref, detail)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetEntity :one
SELECT * FROM entity WHERE node_id = $1;

-- name: FindEntityBySubtypeName :one
-- Entity-resolution dedup lookup against the (subtype, normalized_name) unique.
SELECT * FROM entity WHERE subtype = $1 AND normalized_name = $2;

-- name: UpdateEntityDetail :exec
-- Merge-patches the per-instance detail JSONB (e.g. a tag color edit) using the
-- || concatenation operator so other keys are preserved, not overwritten.
UPDATE entity SET detail = detail || $2 WHERE node_id = $1;
