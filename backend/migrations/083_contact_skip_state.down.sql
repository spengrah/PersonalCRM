BEGIN;

DROP VIEW live_contact;

ALTER TABLE contact
    DROP COLUMN last_skipped_at,
    DROP COLUMN last_skipped_contact_by,
    DROP COLUMN last_skip_reason;

CREATE VIEW live_contact AS
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
    awaiting_reply_until
FROM contact
WHERE deleted_at IS NULL;

COMMIT;
