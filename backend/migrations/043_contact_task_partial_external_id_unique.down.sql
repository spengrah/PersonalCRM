-- 043_contact_task_partial_external_id_unique.down.sql
-- Revert the partial unique index to the original full-table UNIQUE
-- constraint from migration 029. Rolling back after the cutover
-- consumer has started writing pending_remote_create rows creates
-- duplicate external_task_id='' values that the strict UNIQUE
-- constraint cannot accept, so we delete those rows first. This
-- is acceptable for a down-migration: the pending rows would be
-- orphaned anyway once the cutover code is reverted.

DELETE FROM contact_task
WHERE external_task_id = ''
  AND state = 'pending_remote_create';

DROP INDEX IF EXISTS unique_external_task_id;

ALTER TABLE contact_task
    ADD CONSTRAINT unique_external_task_id UNIQUE (external_task_id);
