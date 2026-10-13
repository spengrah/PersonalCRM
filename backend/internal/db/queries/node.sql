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

-- name: AliasInteractionNode :execrows
-- The interaction-node merge write: aliases root interaction node loser_id to
-- the distinct root interaction node winner_id, and nothing else. The loser
-- keeps no tombstone (it stays its source object's identity) and no assertion,
-- observation or content row changes. Only a root (merged_into IS NULL) is
-- aliased, so an alias, once written, is never rewritten. Zero rows when the
-- pair is not two distinct live root interaction nodes.
UPDATE node
SET merged_into = sqlc.arg('winner_id')::uuid
WHERE node.id = sqlc.arg('loser_id')::uuid
  AND node.id <> sqlc.arg('winner_id')::uuid
  AND node.type = 'interaction'
  AND node.merged_into IS NULL
  AND node.deleted_at IS NULL
  AND EXISTS (
      SELECT 1 FROM node w
      WHERE w.id = sqlc.arg('winner_id')::uuid
        AND w.type = 'interaction'
        AND w.merged_into IS NULL
        AND w.deleted_at IS NULL);

-- name: AcquireInteractionCorrectionLocks :exec
-- The correction lock: a transaction-scoped advisory lock keyed by interaction
-- node id, taken in id order so two holders of several never deadlock. The
-- observation step, every assertion write naming an interaction node, and the
-- corrections take it. The key namespace prefix keeps it apart from the other
-- hashtextextended advisory keys; a hash collision only over-serializes.
SELECT pg_advisory_xact_lock(hashtextextended('interaction_correction:' || ids.id::text, 0))
FROM (SELECT DISTINCT unnest(sqlc.arg('node_ids')::uuid[]) AS id ORDER BY 1) ids;

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
