-- Drop the graph identity tables in reverse FK dependency order: venue and
-- entity reference node (and entity references entity_type), so drop the leaf
-- subtype tables first, then entity_type, then node last.

BEGIN;

DROP TABLE venue;
DROP TABLE entity;
DROP TABLE entity_type;
DROP TABLE node;

COMMIT;
