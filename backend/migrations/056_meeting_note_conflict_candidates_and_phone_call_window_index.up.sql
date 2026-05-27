-- 056_meeting_note_conflict_candidates_and_phone_call_window_index.up.sql
-- Mac daemon Phase 2 PR 5: persist the per-candidate participant-overlap
-- snapshot recorded at the moment linkage_state is set to conflict_pending
-- so the resolve-link endpoint can validate user-supplied (kind, id) tuples
-- against the snapshot AND the needs-attention list endpoint can render
-- candidate previews from the persisted snapshot directly.
--
-- Also adds an index backing the FindPhoneCallsInWindow query that the
-- meeting_note linkage handler now uses to extend Step 1's candidate set
-- with phone_call rows in the ±15-minute linkage window. Without a leading
-- started_at index, the BETWEEN query degrades to a seq scan; on a
-- Raspberry Pi, even modest call volumes make this a measurable concern.

-- ============================================================================
-- 1. meeting_note.conflict_candidates JSONB column
-- ============================================================================
ALTER TABLE meeting_note
    ADD COLUMN conflict_candidates JSONB;

COMMENT ON COLUMN meeting_note.conflict_candidates IS
    'Persisted snapshot of the per-candidate participant-overlap table '
    'recorded at the moment linkage_state was set to conflict_pending. '
    'NULL for any other state. Shape: array of {kind, id, occurred_at, '
    'overlap_count} sorted by overlap desc then occurred_at asc. The '
    'GET /api/v1/meeting-notes/needs-attention endpoint projects from '
    'this column directly. The POST /resolve-link endpoint validates '
    'that user-supplied (kind, id) tuples appear in this snapshot.';

-- No CHECK constraint on the shape — JSONB validation lives at the
-- application layer (writers always go through the typed snapshot
-- struct). Future migrations can add a CHECK if shape drift becomes a
-- real risk.

-- No index — the column is only ever read via the primary-key path
-- (GetMeetingNoteByID for resolve-link) or via the
-- linkage-state-filtered list (idx_meeting_note_linkage_state already
-- exists from migration 053).

-- ============================================================================
-- 2. idx_phone_call_started_at — back the FindPhoneCallsInWindow query
-- ============================================================================
-- phone_call has no deleted_at column so no partial predicate is needed.
-- Per-contact timeline queries are backed by idx_phone_call_matched_contact
-- (composite on matched_contact_id, started_at); that composite is NOT
-- usable for a BETWEEN started_at scan without a matched_contact_id
-- predicate. The new linkage-candidate query has no contact filter, so
-- it needs a standalone started_at index.
CREATE INDEX idx_phone_call_started_at
    ON phone_call(started_at);
