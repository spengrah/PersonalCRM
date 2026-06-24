-- Entity-type catalog queries (SP1 graph foundation).

-- name: UpsertEntityType :exec
-- Idempotent entity-type seed support (PR2 seeds the curated subtypes).
INSERT INTO entity_type (key, description, resolution_config, status)
VALUES ($1, $2, $3, $4)
ON CONFLICT (key)
DO UPDATE SET description = EXCLUDED.description,
             resolution_config = EXCLUDED.resolution_config,
             status = EXCLUDED.status;

-- name: GetEntityType :one
SELECT * FROM entity_type WHERE key = $1;
