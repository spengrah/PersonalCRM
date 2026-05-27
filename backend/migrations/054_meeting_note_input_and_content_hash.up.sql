-- ============================================================================
-- 054_meeting_note_input_and_content_hash: add input_hash, resolved_set_hash,
-- last_content_hash, and meeting_at columns to meeting_note
-- ============================================================================
-- Spec: .ai/spec/mac-daemon-phase-2-anarlog-matching.md
--
-- The columns added here power the linkage-detection re-sync algorithm and the
-- /known-ids endpoint:
--
--   * input_hash         SHA-256(JCS({meeting_at, title, sorted(participant_ids)})).
--                        Detects "matching inputs changed" on re-sync without
--                        re-running linkage on every duplicate event.
--   * resolved_set_hash  SHA-256(JCS(sorted(resolved_contact_uuids))). Detects
--                        when tagged-participant→contact resolution changes
--                        even when input_hash is unchanged (e.g. after a user
--                        imports an anarlog_humans candidate).
--   * last_content_hash  Lowercase-hex SHA-256 of the most recent
--                        meeting_note.recorded payload. Powers the daemon's
--                        deterministic delete source_id construction via
--                        /sync/anarlog_sessions/known-ids.
--   * meeting_at         Session start time from the daemon payload's
--                        meeting_at. Drives linkage window math, the
--                        re-sync input_hash recipe, and the conflict UI.
--
-- Defaults exist purely for the migration boundary against any pre-existing
-- meeting_note rows; in steady state every write supplies a real value
-- (validators reject zero meeting_at / empty hashes). The CHECK constraints
-- enforce lowercase-hex SHA-256 shape on the two hash columns.
-- ============================================================================

ALTER TABLE meeting_note
    ADD COLUMN input_hash        TEXT        NOT NULL DEFAULT '',
    ADD COLUMN resolved_set_hash TEXT        NOT NULL DEFAULT '',
    ADD COLUMN last_content_hash TEXT,
    ADD COLUMN meeting_at        TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01T00:00:00Z',
    ADD CONSTRAINT meeting_note_input_hash_check
        CHECK (input_hash = '' OR input_hash ~ '^[a-f0-9]{64}$'),
    ADD CONSTRAINT meeting_note_resolved_set_hash_check
        CHECK (resolved_set_hash = '' OR resolved_set_hash ~ '^[a-f0-9]{64}$');

COMMENT ON COLUMN meeting_note.input_hash IS
    'SHA-256(JCS({meeting_at, title, sorted(participant_ids)})). Used to detect when matching inputs change on re-sync.';
COMMENT ON COLUMN meeting_note.resolved_set_hash IS
    'SHA-256(JCS(sorted(resolved_contact_uuids))). Used to detect when tagged-participant->contact resolution changes (e.g., after a user import), even when input_hash is unchanged.';
COMMENT ON COLUMN meeting_note.last_content_hash IS
    'Lowercase-hex SHA-256 of JCS-canonicalized payload (minus host_id) from the most recent meeting_note.recorded event. Powers GET /sync/anarlog_sessions/known-ids. NULL for legacy rows; daemon falls back to @deleted@unknown sentinel per spec.';
COMMENT ON COLUMN meeting_note.meeting_at IS
    'Session start time from the daemon payload meeting_at. Drives linkage window math, /imports conflict UI, and the input_hash recipe.';
