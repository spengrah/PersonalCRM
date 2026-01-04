-- Map external identifiers to CRM contacts
CREATE TABLE external_identity (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),

    -- The external identifier
    identifier TEXT NOT NULL,           -- Normalized value (e.g., 'john@example.com', '+15551234567')
    identifier_type TEXT NOT NULL,      -- 'email', 'phone', 'telegram', 'imessage_email', 'imessage_phone'
    raw_identifier TEXT,                -- Original format before normalization

    -- Source tracking
    source TEXT NOT NULL,               -- 'gmail', 'imessage', 'telegram', 'gcal', etc.
    source_id TEXT,                     -- Platform-specific ID (e.g., Gmail contact ID)

    -- CRM contact link
    contact_id UUID REFERENCES contact(id) ON DELETE SET NULL,
    match_type TEXT CHECK (match_type IN ('exact', 'fuzzy', 'manual', 'unmatched')),
    match_confidence FLOAT,             -- 0.0-1.0 for fuzzy matches

    -- Metadata
    display_name TEXT,                  -- Name from external source (for unmatched review)
    last_seen_at TIMESTAMPTZ,           -- Last time this identity appeared in sync
    message_count INTEGER DEFAULT 0,    -- How many messages from this identity

    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),

    UNIQUE (identifier, identifier_type, source)
);

-- Fast lookup by identifier (for matching during sync)
CREATE INDEX idx_external_identity_lookup ON external_identity(identifier_type, identifier);

-- Find unmatched identities for review
CREATE INDEX idx_external_identity_unmatched ON external_identity(contact_id) WHERE contact_id IS NULL;

-- Find identities for a contact
CREATE INDEX idx_external_identity_contact ON external_identity(contact_id) WHERE contact_id IS NOT NULL;
