-- Recreate the reminder table (restore from initial schema + migrations)
CREATE TABLE reminder (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    contact_id UUID REFERENCES contact(id) ON DELETE CASCADE,
    title TEXT NOT NULL,
    description TEXT,
    due_date TIMESTAMPTZ NOT NULL,
    completed BOOLEAN NOT NULL DEFAULT FALSE,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ,
    source TEXT NOT NULL DEFAULT 'manual'
);

-- Recreate indexes
CREATE INDEX idx_reminder_contact_id ON reminder(contact_id);
CREATE INDEX idx_reminder_due_date ON reminder(due_date);
CREATE INDEX idx_reminder_completed ON reminder(completed);
CREATE INDEX idx_reminder_deleted_at ON reminder(deleted_at);
