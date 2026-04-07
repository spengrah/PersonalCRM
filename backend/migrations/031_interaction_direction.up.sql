-- 1. Add direction column to interaction
ALTER TABLE interaction ADD COLUMN direction TEXT NOT NULL DEFAULT 'mutual';
ALTER TABLE interaction ADD CONSTRAINT interaction_direction_check
    CHECK (direction IN ('outbound', 'inbound', 'mutual'));

-- 2. Update source constraint to include telegram
ALTER TABLE interaction DROP CONSTRAINT interaction_source_check;
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist', 'telegram'));

-- 3. Add new contact timestamp columns
ALTER TABLE contact ADD COLUMN last_interaction_at TIMESTAMPTZ;
ALTER TABLE contact ADD COLUMN last_outreach_at TIMESTAMPTZ;
ALTER TABLE contact ADD COLUMN last_response_at TIMESTAMPTZ;

-- 4. Backfill: existing last_contacted represents mutual interactions
UPDATE contact
SET last_interaction_at = last_contacted,
    last_outreach_at = last_contacted,
    last_response_at = last_contacted
WHERE last_contacted IS NOT NULL;

-- 5. Indexes for sorting/filtering contact list by new fields
CREATE INDEX idx_contact_last_interaction_at ON contact(last_interaction_at DESC NULLS LAST)
    WHERE deleted_at IS NULL;
CREATE INDEX idx_contact_last_outreach_at ON contact(last_outreach_at DESC NULLS LAST)
    WHERE deleted_at IS NULL;
CREATE INDEX idx_contact_last_response_at ON contact(last_response_at DESC NULLS LAST)
    WHERE deleted_at IS NULL;
