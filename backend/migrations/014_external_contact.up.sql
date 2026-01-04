-- Migration: 014_external_contact
-- Description: Create unified external contact table for Google/iCloud contacts sync
--              and contact enrichment tracking

-- Unified external contact table for Google/iCloud Contacts sync
CREATE TABLE external_contact (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),

    -- Source identification
    source TEXT NOT NULL,                    -- 'google', 'icloud'
    source_id TEXT NOT NULL,                 -- google: resource_name, icloud: record_id
    account_id TEXT,                         -- Google: email, iCloud: NULL

    -- Contact data
    display_name TEXT,
    first_name TEXT,
    last_name TEXT,

    -- Contact methods (JSONB arrays)
    emails JSONB DEFAULT '[]',               -- [{value, type, primary}]
    phones JSONB DEFAULT '[]',               -- [{value, type, primary}]
    addresses JSONB DEFAULT '[]',            -- [{formatted, type}]

    -- Additional fields
    organization TEXT,
    job_title TEXT,
    birthday DATE,
    photo_url TEXT,

    -- CRM matching
    crm_contact_id UUID REFERENCES contact(id) ON DELETE SET NULL,
    match_status TEXT DEFAULT 'unmatched' CHECK (match_status IN (
        'matched',      -- Linked to CRM contact
        'unmatched',    -- No CRM contact found
        'ignored',      -- User chose to ignore
        'imported'      -- Created CRM contact from this
    )),

    -- Deduplication (same person across accounts)
    duplicate_of_id UUID REFERENCES external_contact(id) ON DELETE SET NULL,

    -- Metadata
    etag TEXT,                               -- Google sync etag
    metadata JSONB DEFAULT '{}',             -- Source-specific data
    synced_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Contact enrichment tracking
-- Tracks which CRM contact fields were enriched from external sources
CREATE TABLE contact_enrichment (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    contact_id UUID NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
    source TEXT NOT NULL,                    -- 'google', 'icloud'
    account_id TEXT,                         -- Which account provided this
    field TEXT NOT NULL,                     -- 'profile_photo', 'birthday', 'location', 'method:phone:+1...'
    external_contact_id UUID REFERENCES external_contact(id) ON DELETE SET NULL,
    original_value TEXT,                     -- What was imported (for auditing)
    enriched_at TIMESTAMPTZ DEFAULT NOW()
);

-- Unique constraints via indexes (COALESCE handles NULL account_id)
CREATE UNIQUE INDEX idx_external_contact_unique ON external_contact(source, source_id, COALESCE(account_id, ''));
CREATE UNIQUE INDEX idx_contact_enrichment_unique ON contact_enrichment(contact_id, source, field, COALESCE(account_id, ''));

-- Indexes for external_contact
CREATE INDEX idx_external_contact_source ON external_contact(source);
CREATE INDEX idx_external_contact_source_account ON external_contact(source, account_id);
CREATE INDEX idx_external_contact_crm_contact ON external_contact(crm_contact_id) WHERE crm_contact_id IS NOT NULL;
CREATE INDEX idx_external_contact_unmatched ON external_contact(source, match_status) WHERE match_status = 'unmatched';
CREATE INDEX idx_external_contact_duplicate ON external_contact(duplicate_of_id) WHERE duplicate_of_id IS NOT NULL;
CREATE INDEX idx_external_contact_synced ON external_contact(synced_at);

-- Indexes for contact_enrichment
CREATE INDEX idx_contact_enrichment_contact ON contact_enrichment(contact_id);
CREATE INDEX idx_contact_enrichment_source ON contact_enrichment(source);

-- Trigger for updated_at
CREATE TRIGGER update_external_contact_updated_at BEFORE UPDATE ON external_contact
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
