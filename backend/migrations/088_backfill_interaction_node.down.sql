-- Drops the representability constraint only. The backfilled nodes and every
-- observation's interaction_node_id stay: they are structure, and keeping them
-- lets a re-run of 088 leave every observation on the node it had and create
-- none. Migration 087's down removes the nodes with the column.

BEGIN;

ALTER TABLE interaction DROP CONSTRAINT interaction_node_representable_check;

COMMIT;
