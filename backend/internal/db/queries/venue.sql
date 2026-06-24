-- Venue subtype queries (graph foundation).
--
-- Venue rows have no deleted_at of their own: liveness flows from the parent
-- node's tombstone. So the live reads join node and filter node.deleted_at IS
-- NULL; a venue whose node has been merged/soft-deleted drops from these reads.

-- name: CreateVenue :one
INSERT INTO venue (node_id, kind, source, source_container_id, title)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetVenue :one
SELECT venue.* FROM venue
JOIN node ON node.id = venue.node_id
WHERE venue.node_id = $1 AND node.deleted_at IS NULL;

-- name: FindVenueByContainer :one
-- Looks up the single live venue for a real container via the
-- (source, kind, source_container_id) unique.
SELECT venue.* FROM venue
JOIN node ON node.id = venue.node_id
WHERE venue.source = $1 AND venue.kind = $2 AND venue.source_container_id = $3
  AND node.deleted_at IS NULL;

-- name: UpsertVenue :one
-- Idempotent venue creation for the interaction backfill / live recorders: a
-- re-run for the same container is a no-op that refreshes the title and returns
-- the existing row.
INSERT INTO venue (node_id, kind, source, source_container_id, title)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (source, kind, source_container_id)
DO UPDATE SET title = EXCLUDED.title
RETURNING *;
