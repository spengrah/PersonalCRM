-- 073_comms_message_eligible_indexes.down.sql
-- Drops the two partial indexes added by the up migration. Idempotent
-- (IF EXISTS) so a partial-apply / Force / SetVersion recovery path is robust.
-- Dropping an index destroys no data, so no row-count guard is needed.
DROP INDEX IF EXISTS idx_comms_message_stale_claim;
DROP INDEX IF EXISTS idx_comms_message_unprocessed_eligible;
