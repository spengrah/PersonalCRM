-- Meeting Note queries
-- Spec: .ai/spec/mac-daemon-phase-2-anarlog-matching.md

-- name: InsertMeetingNote :one
-- Inserts a meeting_note staging row. linkage_state must be supplied by the
-- caller (the ingest tx computes it from the linkage detection algorithm);
-- no DB-level default exists.
INSERT INTO meeting_note (
    anarlog_session_id,
    title,
    summary,
    memo,
    participants,
    mac_host_id,
    linked_kind,
    linked_id,
    linkage_state
) VALUES (
    sqlc.arg('anarlog_session_id'),
    sqlc.arg('title'),
    sqlc.arg('summary'),
    sqlc.arg('memo'),
    sqlc.arg('participants'),
    sqlc.arg('mac_host_id'),
    sqlc.arg('linked_kind'),
    sqlc.arg('linked_id'),
    sqlc.arg('linkage_state')
)
RETURNING *;

-- name: GetMeetingNoteBySessionID :one
-- Returns the live (non-soft-deleted) meeting_note row for a given anarlog
-- session UUID. Used by the ingest path for dedup and re-sync detection.
SELECT * FROM meeting_note
WHERE anarlog_session_id = sqlc.arg('anarlog_session_id')
  AND deleted_at IS NULL;
