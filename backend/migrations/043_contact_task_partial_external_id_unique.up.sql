-- 043_contact_task_partial_external_id_unique.up.sql
-- Make unique_external_task_id a partial unique index so the two-step
-- follow-up creation can insert pending_remote_create rows with
-- external_task_id = '' (empty string placeholder). The prior
-- full-table UNIQUE constraint from migration 029 collided on the
-- empty string between concurrent pending rows, which blocked the
-- cutover consumer's step-1 insert whenever more than one follow-up
-- for different contacts was in flight simultaneously.
--
-- The partial index preserves the original invariant ("Todoist task
-- IDs are globally unique in our DB") for all populated IDs; empty
-- strings represent "not yet populated by step-2 worker" and are
-- freely repeatable.

ALTER TABLE contact_task DROP CONSTRAINT IF EXISTS unique_external_task_id;

CREATE UNIQUE INDEX unique_external_task_id
    ON contact_task (external_task_id)
    WHERE external_task_id <> '';
