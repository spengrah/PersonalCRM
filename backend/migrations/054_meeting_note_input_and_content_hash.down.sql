-- Reverses 054_meeting_note_input_and_content_hash.up.sql.
--
-- Destructive: dropping these columns loses all stored hashes and the
-- session start time. Acceptable because the columns are forward-only
-- additions and any partial deployment that needs rollback also rolls
-- back the producers that populate them.

ALTER TABLE meeting_note
    DROP CONSTRAINT IF EXISTS meeting_note_input_hash_check,
    DROP CONSTRAINT IF EXISTS meeting_note_resolved_set_hash_check,
    DROP COLUMN IF EXISTS meeting_at,
    DROP COLUMN IF EXISTS last_content_hash,
    DROP COLUMN IF EXISTS resolved_set_hash,
    DROP COLUMN IF EXISTS input_hash;
