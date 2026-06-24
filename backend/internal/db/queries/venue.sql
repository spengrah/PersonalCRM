-- Venue subtype queries (SP1 graph foundation).

-- name: CreateVenue :one
INSERT INTO venue (node_id, kind, source, source_container_id, title)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetVenue :one
SELECT * FROM venue WHERE node_id = $1;

-- name: FindVenueByContainer :one
-- Looks up the single venue for a real container via the
-- (source, kind, source_container_id) unique.
SELECT * FROM venue
WHERE source = $1 AND kind = $2 AND source_container_id = $3;

-- name: UpsertVenue :one
-- Idempotent venue creation for the PR6 interaction backfill: a re-run for the
-- same container is a no-op that refreshes the title and returns the existing row.
INSERT INTO venue (node_id, kind, source, source_container_id, title)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (source, kind, source_container_id)
DO UPDATE SET title = EXCLUDED.title
RETURNING *;
