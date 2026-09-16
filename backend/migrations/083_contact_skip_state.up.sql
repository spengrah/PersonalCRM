BEGIN;

-- Skip state: one setter (the CRM skip, CadenceUpdater.ApplySkip) and many
-- clearers (every other writer of contact_by). The three columns are written
-- in the same statement as the contact_by advance, so they have one writer
-- by construction and are deliberately NOT added to the derived-writer
-- trigger.
ALTER TABLE contact
    ADD COLUMN last_skipped_at TIMESTAMPTZ NULL,
    ADD COLUMN last_skipped_contact_by DATE NULL,
    ADD COLUMN last_skip_reason TEXT NULL;

CREATE OR REPLACE VIEW live_contact AS
SELECT
    id,
    full_name,
    location,
    birthday,
    how_met,
    cadence,
    last_contacted,
    profile_photo,
    deleted_at,
    created_at,
    updated_at,
    contact_by,
    last_interaction_at,
    last_outreach_at,
    last_response_at,
    awaiting_reply_until,
    last_skipped_at,
    last_skipped_contact_by,
    last_skip_reason
FROM contact
WHERE deleted_at IS NULL;

COMMIT;
