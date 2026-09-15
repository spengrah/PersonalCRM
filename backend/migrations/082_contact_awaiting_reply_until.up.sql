BEGIN;

ALTER TABLE contact ADD COLUMN awaiting_reply_until DATE NULL;

CREATE OR REPLACE FUNCTION reject_unauthorized_derived_contact_write()
RETURNS TRIGGER AS $$
DECLARE
    writer text := current_setting('crm.derived_writer', true);
    got    text := COALESCE(writer, '<unset>');
BEGIN
    IF writer IS DISTINCT FROM 'cadence' THEN
        IF NEW.last_contacted IS DISTINCT FROM OLD.last_contacted THEN
            RAISE EXCEPTION 'derived column contact.last_contacted requires crm.derived_writer=cadence (got %)', got;
        END IF;
        IF NEW.last_interaction_at IS DISTINCT FROM OLD.last_interaction_at THEN
            RAISE EXCEPTION 'derived column contact.last_interaction_at requires crm.derived_writer=cadence (got %)', got;
        END IF;
        IF NEW.last_outreach_at IS DISTINCT FROM OLD.last_outreach_at THEN
            RAISE EXCEPTION 'derived column contact.last_outreach_at requires crm.derived_writer=cadence (got %)', got;
        END IF;
        IF NEW.last_response_at IS DISTINCT FROM OLD.last_response_at THEN
            RAISE EXCEPTION 'derived column contact.last_response_at requires crm.derived_writer=cadence (got %)', got;
        END IF;
        IF NEW.contact_by IS DISTINCT FROM OLD.contact_by THEN
            RAISE EXCEPTION 'derived column contact.contact_by requires crm.derived_writer=cadence (got %)', got;
        END IF;
        IF NEW.awaiting_reply_until IS DISTINCT FROM OLD.awaiting_reply_until THEN
            RAISE EXCEPTION 'derived column contact.awaiting_reply_until requires crm.derived_writer=cadence (got %)', got;
        END IF;
    END IF;

    IF writer IS DISTINCT FROM 'knowledge_cache' THEN
        IF NEW.location IS DISTINCT FROM OLD.location THEN
            RAISE EXCEPTION 'derived column contact.location requires crm.derived_writer=knowledge_cache (got %)', got;
        END IF;
        IF NEW.birthday IS DISTINCT FROM OLD.birthday THEN
            RAISE EXCEPTION 'derived column contact.birthday requires crm.derived_writer=knowledge_cache (got %)', got;
        END IF;
        IF NEW.how_met IS DISTINCT FROM OLD.how_met THEN
            RAISE EXCEPTION 'derived column contact.how_met requires crm.derived_writer=knowledge_cache (got %)', got;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

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
    awaiting_reply_until
FROM contact
WHERE deleted_at IS NULL;

SET LOCAL crm.derived_writer = 'cadence';

-- This is a one-time snapshot of the production WATCHDOG_*_DAYS defaults in
-- backend/internal/config/config.go (WatchdogConfig), not a second writer.
-- Later retuning applies only to new outbound interactions. PostgreSQL casts
-- last_outreach_at::date in the session time zone; a midnight-adjacent outreach
-- may land a day off, which this one-time backfill accepts.
UPDATE contact
SET awaiting_reply_until = last_outreach_at::date + CASE cadence
    WHEN 'weekly' THEN 3
    WHEN 'biweekly' THEN 5
    WHEN 'monthly' THEN 7
    WHEN 'quarterly' THEN 14
    WHEN 'biannual' THEN 21
    WHEN 'annual' THEN 21
END
WHERE deleted_at IS NULL
  AND cadence IS NOT NULL
  AND last_outreach_at IS NOT NULL;

COMMIT;
