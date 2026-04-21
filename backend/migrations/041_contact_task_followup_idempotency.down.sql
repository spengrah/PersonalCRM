-- 041_contact_task_followup_idempotency.down.sql
-- Reverse of 041. Normalizes any 'pending_remote_create' rows back to
-- 'managed' before restoring the old CHECK — without that step, the
-- ADD CONSTRAINT below fails on any row that ever reached the new
-- state.
--
-- We do NOT restore the legacy unique_contact_provider_kind hard
-- constraint that migration 028 declared — migration 029 retired it in
-- favor of unique_contact_provider_cadence (partial, kind='cadence'),
-- which remains in force.

DROP INDEX IF EXISTS idx_contact_task_followup_idempotency;
DROP INDEX IF EXISTS idx_contact_task_followup_unique_live;

UPDATE contact_task
   SET state = 'managed'
 WHERE state = 'pending_remote_create';

ALTER TABLE contact_task DROP CONSTRAINT IF EXISTS contact_task_state_check;
ALTER TABLE contact_task
    ADD CONSTRAINT contact_task_state_check
    CHECK (state IN ('managed', 'unmanaged', 'completed', 'dismissed'));

ALTER TABLE contact_task DROP COLUMN IF EXISTS idempotency_key;
