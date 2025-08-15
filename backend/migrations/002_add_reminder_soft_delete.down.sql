-- Remove indexes
DROP INDEX IF EXISTS idx_reminder_due_date_completed;
DROP INDEX IF EXISTS idx_reminder_deleted_at;

-- Remove deleted_at column
ALTER TABLE reminder DROP COLUMN IF EXISTS deleted_at;

