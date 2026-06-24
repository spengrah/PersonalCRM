-- Guarded removal of the backfilled person nodes.
--
-- The assertion table already exists at this migration level (067 < 068), so a
-- rollback that runs after assertion data has landed must NOT orphan or FK-
-- violate against those rows. A plain "delete all person nodes" down is
-- DISALLOWED for exactly that reason. Instead, delete ONLY person nodes that
-- have become non-load-bearing: not a merge winner still referenced by a
-- preserved loser (the node.merged_into self-FK is restrict), and referenced by
-- NO assertion (as subject or object). Any person node something still points
-- at is left intact, so the down degrades gracefully when the graph has grown
-- beyond this migration's seed.

BEGIN;

DELETE FROM node
WHERE type = 'person'
  AND merged_into IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM node child
    WHERE child.merged_into = node.id
  )
  AND NOT EXISTS (
    SELECT 1 FROM assertion a
    WHERE a.subject_node_id = node.id
       OR a.object_node_id = node.id
  );

COMMIT;
