-- Drops only the constraint, leaving the backfilled nodes in place. A down
-- that deleted the rows the up created would destroy graph state (assertions,
-- relationship signals) that may since have attached to them — this mirrors
-- 068's own guarded-delete philosophy, applied here by simply not touching
-- node rows at all.

BEGIN;

ALTER TABLE contact
    DROP CONSTRAINT contact_id_node_fk;

COMMIT;
