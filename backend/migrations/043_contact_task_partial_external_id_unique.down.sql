-- 043_contact_task_partial_external_id_unique.down.sql
-- Revert the partial unique index to the original full-table UNIQUE
-- constraint from migration 029. Rolling this back while rows with
-- external_task_id = '' exist will fail — callers must clean up
-- pending_remote_create rows before down-migrating past 043.

DROP INDEX IF EXISTS unique_external_task_id;

ALTER TABLE contact_task
    ADD CONSTRAINT unique_external_task_id UNIQUE (external_task_id);
