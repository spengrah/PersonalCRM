-- Remove source column from reminder table

DROP INDEX IF EXISTS idx_reminder_source;

ALTER TABLE reminder DROP COLUMN source;
