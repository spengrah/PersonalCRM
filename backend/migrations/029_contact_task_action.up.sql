-- Migration: Allow multiple action tasks per contact (modify unique constraint)
-- The current constraint allows only one task per (contact_id, provider, kind).
-- For action tasks, we need multiple tasks per contact, so we change to a partial
-- unique constraint that only enforces uniqueness for cadence tasks.

-- Drop existing unique constraint
ALTER TABLE contact_task DROP CONSTRAINT unique_contact_provider_kind;

-- Add partial unique index for cadence only (one cadence task per contact per provider)
CREATE UNIQUE INDEX unique_contact_provider_cadence
ON contact_task (contact_id, provider, kind)
WHERE kind = 'cadence';

-- Add unique constraint on external_task_id (Todoist task IDs are globally unique)
-- This allows upsert to work via ON CONFLICT (external_task_id)
ALTER TABLE contact_task
ADD CONSTRAINT unique_external_task_id UNIQUE (external_task_id);

-- Add CHECK constraint for state to include 'completed'
ALTER TABLE contact_task
ADD CONSTRAINT contact_task_state_check
CHECK (state IN ('managed', 'unmanaged', 'completed'));
