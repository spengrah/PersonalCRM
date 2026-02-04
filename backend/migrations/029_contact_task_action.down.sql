-- Rollback: Restore original unique constraint and remove completed state

-- Drop the partial unique index
DROP INDEX IF EXISTS unique_contact_provider_cadence;

-- Drop the unique constraint on external_task_id
ALTER TABLE contact_task DROP CONSTRAINT IF EXISTS unique_external_task_id;

-- Drop the state check constraint
ALTER TABLE contact_task DROP CONSTRAINT IF EXISTS contact_task_state_check;

-- Restore original unique constraint (one task per contact+provider+kind)
ALTER TABLE contact_task
ADD CONSTRAINT unique_contact_provider_kind UNIQUE (contact_id, provider, kind);

-- Note: Any action tasks created will cause this rollback to fail if they have
-- duplicate (contact_id, provider, kind) combinations. Clean up action tasks first:
-- DELETE FROM contact_task WHERE kind = 'action';
