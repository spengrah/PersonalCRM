-- Entity-type catalog queries (graph foundation).

-- name: UpsertEntityType :exec
-- Idempotent entity-type seed support (the curated subtypes are seeded by the
-- predicate-catalog migration).
INSERT INTO entity_type (key, description, resolution_config, status)
VALUES ($1, $2, $3, $4)
ON CONFLICT (key)
DO UPDATE SET description = EXCLUDED.description,
             resolution_config = EXCLUDED.resolution_config,
             status = EXCLUDED.status;

-- name: GetEntityType :one
SELECT * FROM entity_type WHERE key = $1;
