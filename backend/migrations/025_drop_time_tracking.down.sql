-- Restore time tracking schema
-- Migration: 025_drop_time_tracking

CREATE TABLE time_entry (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    description     TEXT NOT NULL,
    project         TEXT,
    contact_id      UUID REFERENCES contact(id) ON DELETE SET NULL,
    start_time      TIMESTAMPTZ NOT NULL,
    end_time        TIMESTAMPTZ,
    duration_minutes INTEGER,
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    updated_at      TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_time_entry_start_time ON time_entry(start_time DESC);
CREATE INDEX idx_time_entry_contact_id ON time_entry(contact_id) WHERE contact_id IS NOT NULL;
CREATE INDEX idx_time_entry_project ON time_entry(project) WHERE project IS NOT NULL;
CREATE INDEX idx_time_entry_end_time ON time_entry(end_time) WHERE end_time IS NULL;
