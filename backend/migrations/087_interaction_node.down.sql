BEGIN;

DROP FUNCTION resolve_interaction_node(TEXT, TEXT);
DROP FUNCTION interaction_identity_key(TEXT, TEXT);
DROP FUNCTION interaction_source_is_burst(TEXT);

ALTER TABLE interaction DROP COLUMN interaction_node_id;
DROP TABLE interaction_node;

-- Interaction nodes that merged into another interaction node alias it through
-- merged_into; clear the aliases before deleting so the self-FK holds.
UPDATE node SET merged_into = NULL WHERE type = 'interaction';
DELETE FROM node WHERE type = 'interaction';

ALTER TABLE node DROP CONSTRAINT node_type_check;
ALTER TABLE node ADD CONSTRAINT node_type_check
    CHECK (type IN ('person', 'venue', 'entity'));

COMMIT;
