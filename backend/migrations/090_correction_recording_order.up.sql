-- Recording order for the interaction correction recompute (CAD-047).
--
-- A correction recomputes a contact's cadence fields by replaying its effective
-- participation through the normal recording rules, composed with the user's
-- own cadence acts. Manual recording and hand acts overwrite unconditionally,
-- so the replay needs the order in which effects were recorded. Two facts the
-- schema did not hold:
--
--   contact.contact_by_hand_set_at / contact_by_hand_set_value
--       when the user last set contact_by by hand (a cadence edit, a Todoist
--       deadline edit or skip, a CRM skip or its undo) and the value it set.
--
--   contact.awaiting_reply_hand_set_at / awaiting_reply_hand_set_value
--       when the user last set awaiting_reply_until by hand (a CRM skip ends
--       the window) and the value it set.
--
--       Each pair is written in the same statement as the column it records,
--       by the hand-act queries only (ApplyContactByHand, ApplyContactSkip,
--       ApplyContactUndoSkip). Like the skip-state columns (migration 083),
--       they are not derived columns and are not in the derived-writer
--       trigger. Hand acts made before this migration are not recorded.
--
--   interaction.recorded_at
--       when an observation's effect was last recorded: its insert, and every
--       later write that changes its direction or occurred_at (extension,
--       promotion to mutual, re-link). A trigger sets it, so no write path can
--       forget it. Existing rows take created_at.
--
-- Both use clock_timestamp(), one clock, so a hand act and a recording compare
-- in the order they ran.

BEGIN;

ALTER TABLE contact
    ADD COLUMN contact_by_hand_set_at TIMESTAMPTZ NULL,
    ADD COLUMN contact_by_hand_set_value DATE NULL,
    ADD COLUMN awaiting_reply_hand_set_at TIMESTAMPTZ NULL,
    ADD COLUMN awaiting_reply_hand_set_value DATE NULL;

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
    contact_by_hand_set_at,
    contact_by_hand_set_value,
    awaiting_reply_hand_set_at,
    awaiting_reply_hand_set_value
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
