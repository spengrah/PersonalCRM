-- Calendar events synced from Google Calendar
-- Used to track meetings with CRM contacts and update last_contacted

CREATE TABLE calendar_event (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),

    -- Google Calendar identifiers
    gcal_event_id TEXT NOT NULL,
    gcal_calendar_id TEXT NOT NULL DEFAULT 'primary',
    google_account_id TEXT NOT NULL,  -- Which Google account owns this event

    -- Event metadata
    title TEXT,
    description TEXT,
    location TEXT,

    -- Timing
    start_time TIMESTAMPTZ NOT NULL,
    end_time TIMESTAMPTZ NOT NULL,
    all_day BOOLEAN DEFAULT FALSE,

    -- Status
    status TEXT DEFAULT 'confirmed' CHECK (status IN ('confirmed', 'tentative', 'cancelled')),
    user_response TEXT CHECK (user_response IN ('accepted', 'declined', 'tentative', 'needsAction')),

    -- Attendees (JSONB for flexibility)
    organizer_email TEXT,
    attendees JSONB DEFAULT '[]',

    -- CRM links
    matched_contact_ids UUID[] DEFAULT '{}',

    -- Sync metadata
    synced_at TIMESTAMPTZ DEFAULT NOW(),
    last_contacted_updated BOOLEAN DEFAULT FALSE,

    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),

    UNIQUE (gcal_event_id, gcal_calendar_id, google_account_id)
);

-- Index for finding events by matched contacts
CREATE INDEX idx_calendar_event_contacts ON calendar_event USING GIN (matched_contact_ids);

-- Index for upcoming events (non-cancelled)
CREATE INDEX idx_calendar_event_start ON calendar_event(start_time) WHERE status != 'cancelled';

-- Index for past events (for last_contacted updates)
CREATE INDEX idx_calendar_event_end ON calendar_event(end_time DESC) WHERE status != 'cancelled';

-- Index by Google account for multi-account support
CREATE INDEX idx_calendar_event_account ON calendar_event(google_account_id);

-- Index for finding past events that need last_contacted updates
CREATE INDEX idx_calendar_event_needs_update ON calendar_event(end_time)
    WHERE last_contacted_updated = FALSE AND status = 'confirmed';
