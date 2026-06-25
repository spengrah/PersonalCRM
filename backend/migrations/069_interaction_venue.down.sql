-- Reverse the venue model on interaction.
--
-- Drops the venue_id index + column, then removes the venue nodes the backfill
-- created — but only those that have become non-load-bearing (guarded like the
-- person-node down in 068). A venue node still referenced by a live interaction
-- (via venue_id) or by an assertion is left intact, so the down degrades
-- gracefully if the graph has grown beyond this migration's seed.
--
-- Wrapped in an explicit transaction for the same reason as the up: the
-- postgres driver does not auto-wrap, and this drops a column AND deletes data.

BEGIN;

-- Drop the column first so no live interaction.venue_id reference can block the
-- venue/node cleanup below. Dropping the column removes the FK to node, so the
-- subsequent DELETE FROM venue/node only needs to guard against assertions.
DROP INDEX IF EXISTS idx_interaction_venue_id;
ALTER TABLE interaction DROP COLUMN IF EXISTS venue_id;

-- Remove venue subtype rows whose node nothing else references. The venue→node
-- FK is ON DELETE CASCADE, so deleting the node deletes the venue row too;
-- delete the node and let the cascade clear the venue.
DELETE FROM node
WHERE type = 'venue'
  -- Skip merge winners still pointed at by a preserved loser's merged_into
  -- self-FK (restrict): without this guard the delete would FK-violate.
  AND merged_into IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM node child
    WHERE child.merged_into = node.id
  )
  -- Skip nodes any assertion references (subject or object): the assertion→node
  -- FK is restrict, so deleting them would FK-violate / orphan history.
  AND NOT EXISTS (
    SELECT 1 FROM assertion a
    WHERE a.subject_node_id = node.id
       OR a.object_node_id = node.id
  );

COMMIT;
