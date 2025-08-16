-- Rollback: Make contact_id required again in reminders
-- Migration: 004_make_contact_id_optional_in_reminders

-- First, we need to handle any existing reminders with null contact_id
-- For safety, we won't delete them but this migration will fail if null values exist
-- In a real scenario, you'd need to handle this data migration carefully

-- Re-add the NOT NULL constraint
ALTER TABLE reminder ALTER COLUMN contact_id SET NOT NULL;
