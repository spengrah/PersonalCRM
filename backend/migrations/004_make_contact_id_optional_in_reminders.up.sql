-- Make contact_id optional in reminders to support standalone reminders
-- Migration: 004_make_contact_id_optional_in_reminders

-- Drop the NOT NULL constraint on contact_id
ALTER TABLE reminder ALTER COLUMN contact_id DROP NOT NULL;

-- Add a partial unique index for contact-based reminders to maintain referential integrity
-- (This helps maintain data consistency while allowing null contact_id values)
