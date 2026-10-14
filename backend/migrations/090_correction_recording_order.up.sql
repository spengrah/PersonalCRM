-- Facts the interaction correction recompute needs (CAD-047).
--
-- A correction recomputes a contact's cadence fields by replaying its effective
-- participation through the normal recording rules, in the order the effects
-- were recorded, composed with two of the user's own cadence acts. The schema
-- did not hold any of the three facts that takes:
--
--   contact.contact_by_direct_set_at / contact_by_direct_set_value
--       the latest due date the user set directly (a Todoist deadline edit on
--       the contact's cadence task) and when it was set. Written only by
--       ApplyContactByDirectSet, in the statement that sets contact_by. A
--       cadence edit, a profile save, an enrichment update and a skip or undo
--       from either side never write it. A direct set made before this
--       migration is absent.
--
--   contact.awaiting_reply_skip_ended_at
--       when a CRM skip last ended the awaiting-reply window. Written only by
--       ApplyContactSkip, in the statement that ends the window. Nothing
--       clears it: an undo does not reopen the window, and a correction keeps
--       a window the skip ended ended.
--
--       Like the skip-state columns (migration 083), these are not derived
--       columns and are not in the derived-writer trigger.
--
--   interaction.recorded_at
--       when an observation's effect was last recorded: its insert, and every
--       later write that changes its direction or occurred_at (extension,
--       promotion to mutual). A trigger sets it, so no write path can omit
--       it. Existing rows take created_at.
--
-- All three use clock_timestamp(), one clock, so a recording and a user act
-- compare in the order they ran.

BEGIN;

ALTER TABLE contact
    ADD COLUMN contact_by_direct_set_at TIMESTAMPTZ NULL,
    ADD COLUMN contact_by_direct_set_value DATE NULL,
    ADD COLUMN awaiting_reply_skip_ended_at TIMESTAMPTZ NULL;

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
    last_skip_reason,
    contact_by_direct_set_at,
    contact_by_direct_set_value,
    awaiting_reply_skip_ended_at
FROM contact
WHERE deleted_at IS NULL;

ALTER TABLE interaction ADD COLUMN recorded_at TIMESTAMPTZ;
UPDATE interaction SET recorded_at = COALESCE(created_at, NOW());
ALTER TABLE interaction ALTER COLUMN recorded_at SET NOT NULL;

CREATE FUNCTION set_interaction_recorded_at()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT'
       OR NEW.direction IS DISTINCT FROM OLD.direction
       OR NEW.occurred_at IS DISTINCT FROM OLD.occurred_at THEN
        NEW.recorded_at := clock_timestamp();
    ELSE
        NEW.recorded_at := OLD.recorded_at;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER set_interaction_recorded_at
    BEFORE INSERT OR UPDATE ON interaction
    FOR EACH ROW EXECUTE FUNCTION set_interaction_recorded_at();

COMMIT;
