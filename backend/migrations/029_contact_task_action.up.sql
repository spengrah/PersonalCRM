-- Migration: Allow multiple action tasks per contact (modify unique constraint)
-- The current constraint allows only one task per (contact_id, provider, kind).
-- For action tasks, we need multiple tasks per contact, so we change to a partial
-- unique constraint that only enforces uniqueness for cadence tasks.

-- Drop existing unique constraint
ALTER TABLE contact_task DROP CONSTRAINT unique_contact_provider_kind;

-- Add partial unique constraint for cadence only (one cadence task per contact per provider)
CREATE UNIQUE INDEX unique_contact_provider_cadence
ON contact_task (contact_id, provider, kind)
WHERE kind = 'cadence';

-- Action tasks have no uniqueness constraint (multiple allowed per contact)

-- Add CHECK constraint for state to include 'completed'
-- First drop any existing check constraint on state (if any)
-- Then add the new one
ALTER TABLE contact_task
ADD CONSTRAINT contact_task_state_check
CHECK (state IN ('managed', 'unmanaged', 'completed'));
