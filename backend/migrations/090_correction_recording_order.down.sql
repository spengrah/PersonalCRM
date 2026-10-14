BEGIN;

DROP TRIGGER set_interaction_recorded_at ON interaction;
DROP FUNCTION set_interaction_recorded_at();
ALTER TABLE interaction DROP COLUMN recorded_at;

DROP VIEW live_contact;

ALTER TABLE contact
    DROP COLUMN contact_by_direct_set_at,
    DROP COLUMN contact_by_direct_set_value;

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
    awaiting_reply_until,
    last_skipped_at,
    last_skipped_contact_by,
    last_skip_reason
FROM contact
WHERE deleted_at IS NULL;

COMMIT;
