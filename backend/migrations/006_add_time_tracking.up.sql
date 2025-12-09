-- Time Tracking Migration
-- Migration: 006_add_time_tracking

-- Time entry table - for tracking time spent on activities
CREATE TABLE time_entry (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    description     TEXT NOT NULL,
    project         TEXT,  -- Optional project/category name
    contact_id      UUID REFERENCES contact(id) ON DELETE SET NULL,  -- Optional link to contact
    start_time      TIMESTAMPTZ NOT NULL,
    end_time        TIMESTAMPTZ,  -- NULL if currently running
    duration_minutes INTEGER,  -- Calculated duration in minutes (cached for performance)
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    updated_at      TIMESTAMPTZ DEFAULT NOW()
);

-- Indexes for time entries
CREATE INDEX idx_time_entry_start_time ON time_entry(start_time DESC);
CREATE INDEX idx_time_entry_contact_id ON time_entry(contact_id) WHERE contact_id IS NOT NULL;
CREATE INDEX idx_time_entry_project ON time_entry(project) WHERE project IS NOT NULL;
CREATE INDEX idx_time_entry_end_time ON time_entry(end_time) WHERE end_time IS NULL;  -- For finding running timers
