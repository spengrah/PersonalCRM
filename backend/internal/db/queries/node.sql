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

-- name: TestCountVenueNodes :one
-- Test-only: counts venue-type nodes. Used by the venue backfill test.
SELECT COUNT(*) FROM node WHERE type = 'venue';

-- name: TestCountOrphanVenueNodes :one
-- Test-only: counts venue-type nodes that no live interaction references via
-- venue_id. Used by the venue backfill test to assert the no-orphan-node guard.
SELECT COUNT(*) FROM node nd
WHERE nd.type = 'venue'
  AND NOT EXISTS (SELECT 1 FROM interaction i WHERE i.venue_id = nd.id);
