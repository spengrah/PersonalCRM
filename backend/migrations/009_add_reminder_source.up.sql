-- Add source column to reminder table to distinguish auto-generated from manual reminders
-- Values: 'auto' for scheduler-generated, 'manual' for user-created

ALTER TABLE reminder ADD COLUMN source TEXT DEFAULT 'manual'
    CHECK (source IN ('auto', 'manual'));

-- Backfill existing auto-generated reminders by title pattern matching
-- Auto-generated reminders have titles like "Reach out to {name} ({cadence})"
UPDATE reminder
SET source = 'auto'
WHERE title LIKE 'Reach out to %' AND source = 'manual';

-- Add index for efficient source-based queries
CREATE INDEX idx_reminder_source ON reminder(source);
