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
  -- Skip merge LOSERS: they carry merged_into (and are tombstoned). Deleting a
  -- merged-away node would silently drop merge history.
  AND merged_into IS NULL
  -- Skip merge WINNERS still pointed at by a preserved loser's merged_into
  -- self-FK (restrict): a winner has merged_into NULL and may have no assertion,
  -- so without this guard the delete would FK-violate against the loser's
  -- merged_into reference.
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
