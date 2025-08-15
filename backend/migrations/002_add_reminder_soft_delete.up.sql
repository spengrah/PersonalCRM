-- Add deleted_at column to reminder table for soft deletes
ALTER TABLE reminder ADD COLUMN deleted_at TIMESTAMPTZ;

-- Add index for deleted_at column
CREATE INDEX idx_reminder_deleted_at ON reminder(deleted_at);

-- Add index for due reminders query optimization
CREATE INDEX idx_reminder_due_date_completed ON reminder(due_date, completed) WHERE deleted_at IS NULL;

