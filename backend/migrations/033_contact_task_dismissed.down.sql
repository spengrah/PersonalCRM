-- Revert: drop the constraint and restore the pre-033 version.
-- Any existing 'dismissed' rows are coerced to 'completed' so the constraint
-- accepts them. This is a best-effort down migration for dev; production
-- should not roll this back once it has taken effect.
UPDATE contact_task SET state = 'completed' WHERE state = 'dismissed';
ALTER TABLE contact_task DROP CONSTRAINT contact_task_state_check;
ALTER TABLE contact_task ADD CONSTRAINT contact_task_state_check
    CHECK (state IN ('managed', 'unmanaged', 'completed'));
