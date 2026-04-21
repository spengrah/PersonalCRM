-- 043_contact_task_partial_external_id_unique.down.sql
-- Revert the partial unique index to the original full-table UNIQUE
-- constraint from migration 029. Rolling back after the cutover
-- consumer has been running creates duplicate external_task_id=''
-- values that the strict UNIQUE constraint cannot accept. Delete ALL
-- follow-up rows with empty external_task_id before restoring the
-- constraint — the close-while-pending race path can leave 'completed'
-- rows with empty external_task_id until the create worker finalizes
-- them, not just 'pending_remote_create' rows. These rows would be
-- orphaned by the rollback anyway (the cutover code that manages them
-- is the code being reverted).

DELETE FROM contact_task
WHERE external_task_id = ''
  AND kind = 'follow_up';

DROP INDEX IF EXISTS unique_external_task_id;

ALTER TABLE contact_task
    ADD CONSTRAINT unique_external_task_id UNIQUE (external_task_id);
