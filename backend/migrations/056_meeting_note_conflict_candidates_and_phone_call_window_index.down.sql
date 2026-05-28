-- Down migration. Both objects are additive in the up; reverting is safe
-- regardless of whether rows have populated conflict_candidates.

DROP INDEX IF EXISTS idx_phone_call_started_at;

ALTER TABLE meeting_note
    DROP COLUMN IF EXISTS conflict_candidates;
