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

-- name: AcquireVenueContainerLock :exec
-- Takes a transaction-scoped advisory lock keyed on a venue container so two
-- live recorders resolving the SAME (source, kind, container) serialize on
-- creation — exactly one node+venue pair is created and no orphan node leaks.
-- hashtextextended folds the container string into the bigint advisory-lock key
-- space; a rare hash collision only over-serializes two unrelated containers (a
-- perf cost), never under-serializes (a correctness cost). Mirrors the
-- per-source_ref aggregation lock in interaction.sql. The lock auto-releases on
-- commit/rollback.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg('lock_key')::text, 0));

-- name: CreateVenueNode :one
-- Creates the node + venue pair for a container in one statement with a
-- caller-supplied deterministic node id (uuid_generate_v5 of the container,
-- matching the migration backfill). Both inserts are ON CONFLICT DO NOTHING so a
-- concurrent winner or a re-run is a no-op without orphaning a node. Returns the
-- venue node id on a fresh create; returns no row when the venue already existed
-- (caller falls back to FindVenueByContainer under the advisory lock).
WITH ins_node AS (
    INSERT INTO node (id, type, canonical_label)
    VALUES (sqlc.arg('node_id'), 'venue', sqlc.arg('canonical_label'))
    ON CONFLICT (id) DO NOTHING
    RETURNING id
)
INSERT INTO venue (node_id, kind, source, source_container_id, title)
SELECT sqlc.arg('node_id'), sqlc.arg('kind'), sqlc.arg('source'),
       sqlc.arg('source_container_id'), sqlc.narg('title')
ON CONFLICT (source, kind, source_container_id) DO NOTHING
RETURNING node_id;
